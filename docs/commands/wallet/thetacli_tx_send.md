## thetacli tx send

Send tokens

### Synopsis

Send tokens

```
thetacli tx send [flags]
```

### Examples

```
thetacli tx send --chain="privatenet" --from=2E833968E5bB786Ae419c4d13189fB081Cc43bab --to=9F1233798E905E173560071255140b4A8aBd3Ec6 --theta=10 --tfuel=900000 --seq=1
```

### Options

```
      --chain string    Chain ID
      --fee string      Fee (default "1000000000000wei")
      --from string     Address to send from
  -h, --help            help for send
      --seq uint        Sequence number of the transaction
      --tfuel string    TFuel amount (default "0")
      --theta string    Theta amount (default "0")
      --to string       Address to send to
      --wallet string   Wallet type (soft|nano) (default "soft")
```

### Options inherited from parent commands

```
      --config string   config path (default is /Users/<username>/.thetacli) (default "/Users/<username>/.thetacli")
```

### SEE ALSO

* [thetacli tx](thetacli_tx.md)	 - Manage transactions

###### Auto generated by spf13/cobra on 24-Jan-2019