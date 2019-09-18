package netsync

import (
	"container/heap"
	"container/list"
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/spf13/viper"
	"github.com/thetatoken/theta/blockchain"
	"github.com/thetatoken/theta/common"
	"github.com/thetatoken/theta/common/util"
	"github.com/thetatoken/theta/core"
	"github.com/thetatoken/theta/dispatcher"

	log "github.com/sirupsen/logrus"
)

const RequestTimeout = 10 * time.Second
const Expiration = 300 * time.Second
const MinInventoryRequestInterval = 3 * time.Second
const MaxInventoryRequestInterval = 30 * time.Second
const RequestQuotaPerSecond = 100

type RequestState uint8

const (
	RequestToSendDataReq = iota
	RequestWaitingDataResp
	RequestToSendBodyReq
	RequestWaitingBodyResp
)

type PendingBlock struct {
	hash       common.Hash
	block      *core.Block
	header     *core.BlockHeader
	peers      []string
	lastUpdate time.Time
	createdAt  time.Time
	status     RequestState
}

func NewPendingBlock(x common.Hash, peerIds []string) *PendingBlock {
	return &PendingBlock{
		hash:       x,
		lastUpdate: time.Now(),
		createdAt:  time.Now(),
		peers:      peerIds,
		status:     RequestToSendDataReq, //RequestToSendHeaderReq,
	}
}

func (pb *PendingBlock) HasTimedOut() bool {
	return time.Since(pb.lastUpdate) > RequestTimeout
}

func (pb *PendingBlock) HasExpired() bool {
	return time.Since(pb.createdAt) > Expiration
}

func (pb *PendingBlock) UpdateTimestamp() {
	pb.lastUpdate = time.Now()
}

type HeaderHeap []*PendingBlock

func (h HeaderHeap) Len() int            { return len(h) }
func (h HeaderHeap) Less(i, j int) bool  { return h[i].header.Height < h[j].header.Height }
func (h HeaderHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *HeaderHeap) Push(x interface{}) { *h = append(*h, x.(*PendingBlock)) }
func (h *HeaderHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

type RequestManager struct {
	logger *log.Entry

	ticker *time.Ticker
	quota  int

	wg      *sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool

	syncMgr    *SyncManager
	chain      *blockchain.Chain
	dispatcher *dispatcher.Dispatcher

	lastInventoryRequest time.Time

	mu                      *sync.RWMutex
	pendingBlocks           *list.List
	pendingBlocksByHash     map[string]*list.Element
	pendingBlocksByParent   map[string][]*core.Block
	pendingBlocksWithHeader *HeaderHeap

	endHashCache      []common.Bytes
	blockRequestCache []common.Bytes
}

func NewRequestManager(syncMgr *SyncManager) *RequestManager {
	rm := &RequestManager{
		ticker: time.NewTicker(1 * time.Second),
		quota:  RequestQuotaPerSecond,

		wg: &sync.WaitGroup{},

		lastInventoryRequest: time.Unix(0, 0),

		syncMgr:    syncMgr,
		chain:      syncMgr.chain,
		dispatcher: syncMgr.dispatcher,

		mu:                      &sync.RWMutex{},
		pendingBlocks:           list.New(),
		pendingBlocksByHash:     make(map[string]*list.Element),
		pendingBlocksByParent:   make(map[string][]*core.Block),
		pendingBlocksWithHeader: &HeaderHeap{},
	}

	logger := util.GetLoggerForModule("request")
	if viper.GetBool(common.CfgLogPrintSelfID) {
		logger = logger.WithFields(log.Fields{"id": rm.syncMgr.consensus.ID()})
	}
	rm.logger = logger

	return rm
}

func (rm *RequestManager) mainLoop() {
	defer rm.wg.Done()

	for {
		select {
		case <-rm.ctx.Done():
			rm.stopped = true
			return
		case <-rm.ticker.C:
			rm.quota = RequestQuotaPerSecond
			rm.tryToDownload()
		}
	}
}

func (rm *RequestManager) Start(ctx context.Context) {
	c, cancel := context.WithCancel(ctx)
	rm.ctx = c
	rm.cancel = cancel

	rm.resumePendingBlocks()

	rm.wg.Add(1)
	go rm.mainLoop()
}

func (rm *RequestManager) Stop() {
	rm.ticker.Stop()
	rm.cancel()
}

func (rm *RequestManager) Wait() {
	rm.wg.Wait()
}

func (rm *RequestManager) buildInventoryRequest() dispatcher.InventoryRequest {
	tip := rm.syncMgr.consensus.GetTip(true)
	lfb := rm.syncMgr.consensus.GetLastFinalizedBlock()

	// Build expontially backoff starting hashes:
	// https://en.bitcoin.it/wiki/Protocol_documentation#getblocks
	starts := []string{}
	step := 1

	// Start at the top of the chain and work backwards.
	for index := tip.Height; index > lfb.Height; index -= uint64(step) {
		// Push top 10 indexes first, then back off exponentially.
		if tip.Height-index >= 10 {
			step *= 2
		}
		// Check overflow
		if uint64(step) > index || step <= 0 {
			break
		}

		blocks := rm.syncMgr.chain.FindBlocksByHeight(index)
		for _, b := range blocks {
			starts = append(starts, b.Hash().Hex())
		}
	}

	//  Push last finalized block.
	starts = append(starts, lfb.Hash().Hex())

	return dispatcher.InventoryRequest{
		ChannelID: common.ChannelIDBlock,
		Starts:    starts,
	}
}

func (rm *RequestManager) tryToDownload() {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	hasUndownloadedBlocks := rm.pendingBlocks.Len() > 0 || len(rm.pendingBlocksByHash) > 0 || len(rm.pendingBlocksByParent) > 0 || rm.pendingBlocksWithHeader.Len() > 0
	minIntervalPassed := time.Since(rm.lastInventoryRequest) >= MinInventoryRequestInterval
	maxIntervalPassed := time.Since(rm.lastInventoryRequest) >= MaxInventoryRequestInterval

	if maxIntervalPassed || (hasUndownloadedBlocks && minIntervalPassed) {
		if hasUndownloadedBlocks && rm.pendingBlocks.Len() > 1 {
			rm.logger.WithFields(log.Fields{
				"pending block hashes": rm.pendingBlocks.Len() - len(rm.pendingBlocksByParent),
				"orphan blocks":        len(rm.pendingBlocksByParent),
				"current chain tip":    rm.syncMgr.consensus.GetTip(true).Hash().Hex(),
			}).Info("Sync progress")
		}

		rm.lastInventoryRequest = time.Now()
		req := rm.buildInventoryRequest()

		rm.logger.WithFields(log.Fields{
			"channelID": req.ChannelID,
			"starts":    req.Starts,
			"end":       req.End,
		}).Debug("Sending inventory request")

		rm.syncMgr.dispatcher.GetInventory([]string{}, req)
	}
	rm.downloadBlockFromHeader(rm.quota)
	rm.downloadBlockFromHash(rm.quota)
}

//compatible with older version, download block from hash
func (rm *RequestManager) downloadBlockFromHash(quota int) {
	//loop over downloaded hash
	var curr *list.Element
	elToRemove := []*list.Element{}
	for curr = rm.pendingBlocks.Front(); quota != 0 && curr != nil; curr = curr.Next() {
		pendingBlock := curr.Value.(*PendingBlock)
		if pendingBlock.HasExpired() {
			elToRemove = append(elToRemove, curr)
			continue
		}
		if pendingBlock.header != nil {
			continue
		}
		if len(pendingBlock.peers) == 0 {
			continue
		}
		if pendingBlock.status == RequestToSendDataReq ||
			(pendingBlock.status == RequestWaitingDataResp && pendingBlock.HasTimedOut()) {
			randomPeerID := pendingBlock.peers[rand.Intn(len(pendingBlock.peers))]
			request := dispatcher.DataRequest{
				ChannelID: common.ChannelIDBlock,
				Entries:   []string{pendingBlock.hash.String()},
			}
			rm.logger.WithFields(log.Fields{
				"channelID":       request.ChannelID,
				"request.Entries": request.Entries,
				"peer":            randomPeerID,
			}).Debug("Sending data request")
			rm.syncMgr.dispatcher.GetData([]string{randomPeerID}, request)
			pendingBlock.UpdateTimestamp()
			pendingBlock.status = RequestWaitingDataResp
			quota--
		}
	}

	for _, el := range elToRemove {
		pendingBlock := el.Value.(*PendingBlock)
		hash := pendingBlock.hash.Hex()
		rm.logger.WithFields(log.Fields{
			"block": hash,
		}).Debug("Removing outdated block")
		rm.removeEl(el)
	}
}

//download block from header
func (rm *RequestManager) downloadBlockFromHeader(quota int) {
	backup := &HeaderHeap{}
	for rm.pendingBlocksWithHeader.Len() > 0 && quota != 0 {
		pendingBlock := heap.Pop(rm.pendingBlocksWithHeader).(*PendingBlock)
		if pendingBlock.HasExpired() {
			pendingBlock.header = nil
			continue
		}
		if pendingBlock.block != nil {
			continue
		}
		if len(pendingBlock.peers) == 0 {
			continue
		}
		if pendingBlock.status == RequestToSendBodyReq ||
			(pendingBlock.status == RequestWaitingBodyResp && pendingBlock.HasTimedOut()) {
			randomPeerID := pendingBlock.peers[rand.Intn(len(pendingBlock.peers))]
			request := dispatcher.DataRequest{
				ChannelID: common.ChannelIDBlock,
				Entries:   []string{pendingBlock.hash.String()},
			}
			rm.logger.WithFields(log.Fields{
				"channelID":       request.ChannelID,
				"request.Entries": request.Entries,
				"peer":            randomPeerID,
			}).Debug("Sending data request")
			rm.syncMgr.dispatcher.GetData([]string{randomPeerID}, request)
			pendingBlock.UpdateTimestamp()
			pendingBlock.status = RequestWaitingBodyResp
			quota--
		}
		heap.Push(backup, pendingBlock)
	}
	rm.pendingBlocksWithHeader = backup
}

func (rm *RequestManager) removeEl(el *list.Element) {
	pendingBlock := el.Value.(*PendingBlock)
	hash := pendingBlock.hash.Hex()

	delete(rm.pendingBlocksByHash, hash)

	if pendingBlock.block != nil {
		parent := pendingBlock.block.Parent.Hex()
		if blocks, ok := rm.pendingBlocksByParent[parent]; ok {
			found := -1
			for idx, block := range blocks {
				if block.Hash() == pendingBlock.block.Hash() {
					found = idx
					break
				}
			}
			if found != -1 {
				blocks = append(blocks[:found], blocks[found+1:]...)
				rm.pendingBlocksByParent[parent] = blocks
			}
			if len(rm.pendingBlocksByParent[parent]) == 0 {
				delete(rm.pendingBlocksByParent, parent)
			}
		}
	}

	rm.pendingBlocks.Remove(el)
}

func (rm *RequestManager) AddHash(x common.Hash, peerIDs []string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.addHash(x, peerIDs)
}

func (rm *RequestManager) addHash(x common.Hash, peerIDs []string) {
	if _, err := rm.chain.FindBlock(x); err == nil {
		return
	}

	var pendingBlockEl *list.Element
	var pendingBlock *PendingBlock
	pendingBlockEl, ok := rm.pendingBlocksByHash[x.String()]
	if !ok {
		pendingBlock = NewPendingBlock(x, peerIDs)
		pendingBlockEl = rm.pendingBlocks.PushBack(pendingBlock)
		rm.pendingBlocksByHash[x.String()] = pendingBlockEl
	}
	// Add peerIDs to pendingBlock.peers
	pendingBlock = pendingBlockEl.Value.(*PendingBlock)
	if pendingBlock.block != nil {
		return
	}
	for _, xid := range peerIDs {
		found := false
		for _, id := range pendingBlock.peers {
			if id == xid {
				found = true
				break
			}
		}
		if !found {
			pendingBlock.peers = append(pendingBlock.peers, xid)
		}
	}
}

func (rm *RequestManager) AddHeader(header *core.BlockHeader) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if _, err := rm.chain.FindBlock(header.Hash()); err == nil {
		rm.logger.Debug("this block is already downloaded")
		return
	}
	if _, ok := rm.pendingBlocksByHash[header.Hash().String()]; !ok {
		rm.addHash(header.Hash(), []string{})
	}
	if pendingBlockEl, ok := rm.pendingBlocksByHash[header.Hash().String()]; ok {
		pendingBlock := pendingBlockEl.Value.(*PendingBlock)
		pendingBlock.header = header
		pendingBlock.status = RequestToSendBodyReq
	}
	heap.Push(rm.pendingBlocksWithHeader, header)
}

func (rm *RequestManager) AddBlock(block *core.Block) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if _, ok := rm.pendingBlocksByHash[block.Hash().String()]; !ok {
		rm.addHash(block.Hash(), []string{})
	}
	if pendingBlockEl, ok := rm.pendingBlocksByHash[block.Hash().String()]; ok {
		pendingBlock := pendingBlockEl.Value.(*PendingBlock)
		//check txHash with header
		if pendingBlock.header != nil && core.CalculateRootHash(block.Txs) != pendingBlock.header.TxHash {
			rm.logger.WithFields(log.Fields{
				"pending block hash": pendingBlock.hash.Hex(),
			}).Info("TxHash doesn't match with header ")
			return
		}
		pendingBlock.block = block
	}
	parent := block.Parent
	if _, err := rm.chain.FindBlock(parent); err == nil {
		rm.dumpReadyBlocks(block)
		return
	}
	byParents, ok := rm.pendingBlocksByParent[parent.String()]
	if !ok {
		byParents = []*core.Block{}
	}
	found := false
	for _, child := range byParents {
		if child.Hash() == block.Hash() {
			found = true
			break
		}
	}
	if !found {
		byParents = append(byParents, block)
	}
	rm.pendingBlocksByParent[parent.String()] = byParents
}

func (rm *RequestManager) dumpAllReadyBlocks() {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	pendings := []*list.Element{}
	for _, pendingBlockEl := range rm.pendingBlocksByHash {
		pendings = append(pendings, pendingBlockEl)
	}
	for _, pendingBlockEl := range pendings {
		pendingBlock := pendingBlockEl.Value.(*PendingBlock)
		block := pendingBlock.block
		if block == nil {
			continue
		}
		if eb, err := rm.chain.FindBlock(block.Parent); err == nil && !eb.Status.IsPending() {
			rm.dumpReadyBlocks(block)
		}
	}
}

// resumePendingBlocks is called during process start to resume blocks that are already downloaded
// but are not yet processed by consensus engine.
func (rm *RequestManager) resumePendingBlocks() {
	lfb := rm.syncMgr.consensus.GetLastFinalizedBlock()
	queue := []*core.ExtendedBlock{lfb}
	for len(queue) > 0 {
		block := queue[0]
		queue = queue[1:]
		if block.Status.IsPending() {
			rm.AddBlock(block.Block)
		}
		for _, hash := range block.Children {
			child, err := rm.chain.FindBlock(hash)
			if err != nil {
				logger.Panic(err)
			}
			queue = append(queue, child)
		}
	}
}

func (rm *RequestManager) dumpReadyBlocks(block *core.Block) {
	queue := []*core.Block{block}
	for len(queue) > 0 {
		block := queue[0]
		hash := block.Hash().String()
		queue = queue[1:]

		if children, ok := rm.pendingBlocksByParent[hash]; ok {
			queue = append(queue, children...)
			delete(rm.pendingBlocksByParent, hash)
		}

		if pendingBlockEl, ok := rm.pendingBlocksByHash[hash]; ok {
			rm.pendingBlocks.Remove(pendingBlockEl)
			delete(rm.pendingBlocksByHash, hash)
		}

		rm.chain.AddBlock(block)

		queueHash := []string{}
		for _, b := range queue {
			queueHash = append(queueHash, b.Hash().Hex())
		}
		rm.syncMgr.PassdownMessage(block)
	}
}
