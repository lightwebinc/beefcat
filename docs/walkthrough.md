# A run on mainnet

One sender, one receiver on another host, real postage on BSV mainnet. The
output below is what the commands printed. Host names and addresses are
replaced with example ones, long ids are shortened in places with an
ellipsis, and step 6 shows only the lines that matter. The bytes of one
such transfer are taken apart in [capture.md](capture.md).

| Fact | Value |
| --- | --- |
| Sender's identity key | `03c1873fcaf504adbbf2746fb36fd8321180c022bd4b20a270726981009911c982` |
| Postage | funding transaction [`f67988ab…08ef1cce`](https://whatsonchain.com/tx/f67988abbbd72aa18cbb5d172be280a1b9e568bf933e2e6f277fe32808ef1cce): 64 outputs of one satoshi, 3,903 bytes, block 975,787 |
| Cost of that postage | 455 satoshis: 64 in the outputs and 391 in miner fee |
| What it pays for | 64 carriers, up to 256 MiB of data |
| On chain because of the data | nothing |

## 1. The sender buys postage, once

```console
sender$ beefcat init
created /home/user/.beefcat
identity     03c1873fcaf504adbbf2746fb36fd8321180c022bd4b20a270726981009911c982
fund address 19QbgoHgZZiiVP3Ja5Ps96CThdcYLvp2hp (main)
```

A payment of 10,000 satoshis to the fund address, from an ordinary
wallet, is imported once it is mined. beefcat reads it from the chain
view and checks its proof against the header source before it counts.

```console
sender$ beefcat fund -n 64 -txid <the payment's txid>
beefcat: imported 1 output(s), 10000 sat, mined at height 975786; pool 1 output(s), 10000 sat
beefcat: funding tree f67988ab…08ef1cce: 64 output(s) of 1 sat
beefcat: funding tree f67988ab…08ef1cce: settling via arcade:https://arcade.gorillapool.io (3903 bytes)
beefcat: funding tree f67988ab…08ef1cce: mined at height 975787
postage for 64 carrier(s), up to 268435456 bytes of data; pool 1 output(s), 9545 sat
```

That is the only transaction this walkthrough puts on the chain.

## 2. Send and verify

The receiver has no key, no coin and no home. It pins the sender's
identity key and takes its block headers from a source it chose, here the
default public one.

```console
receiver$ beefcat -l 9000 -k 03c1873fcaf504adbbf2746fb36fd8321180c022bd4b20a270726981009911c982 > received.bin
beefcat: listening on [::]:9000; proofs are checked against woc:main
beefcat: connection from 192.0.2.10:47404
beefcat: VERIFIED 5 carrier(s), 20000000 bytes in 596ms
  sender      03c1873fcaf504adbbf2746fb36fd8321180c022bd4b20a270726981009911c982 (pinned with -k)
  stream      13be56a91f671495016c47de481a325a
  postage     funding transaction f67988abbbd72aa18cbb5d172be280a1b9e568bf933e2e6f277fe32808ef1cce, mined at height 975787
  headers     woc:main
  retraction  not checked (-unspent)
```

```console
sender$ beefcat receiver.example.com 9000 < file.bin
beefcat: sent 5 carrier(s), 20000000 bytes of data (20023104 on the wire) in 77ms; stream 13be56a91f671495016c47de481a325a; postage left for 35 carrier(s)
beefcat: the far end says: VERIFIED carriers=5 bytes=20000000
```

`received.bin` and `file.bin` have the same SHA-256. Sending asked nothing
of the chain. Of the receiver's 596 ms, most is one request for a block
header; verifying a gigabyte takes about a second
([usage.md](usage.md#speed)).

## 3. The receiver expected someone else

The same send, to a receiver that pinned another key. The data is
genuine, and it is refused: nothing is written, both ends say so, and
both exit with status 4.

```console
receiver$ beefcat -l 9000 -k 0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798 > received.bin

!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
!!  BEEFCAT REFUSED THIS DATA: UNTRUSTED SENDER
!!  REFUSED-UNTRUSTED at carrier 0: this data verified, but its sender
!!  03c1873fcaf504adbbf2746fb36fd8321180c022bd4b20a270726981009911c982 is
!!  not a key you pinned
!!  Nothing was written.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
beefcat: REFUSED-UNTRUSTED carrier=0
```

```console
sender$ beefcat receiver.example.com 9000 < file.bin

!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
!!  THE FAR END REFUSED THIS DATA: IT DOES NOT TRUST THIS SENDER
!!  beefcat REFUSED-UNTRUSTED carrier=0 this data verified, but its sender
!!  03c1873fcaf504adbbf2746fb36fd8321180c022bd4b20a270726981009911c982 is
!!  not a key you pinned
!!  Give the far end this home's identity key (beefcat status) to pin with
!!  -k.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
```

## 4. The stream is cut short

A stream written to a file with `-o`, then truncated. The end of a stream
is signed, so a stream without its last carrier is refused, with status 3.

```console
receiver$ head -c 2500000 stream.bcat | beefcat verify -k 03c1873f…9911c982 > out.bin

!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
!!  BEEFCAT REFUSED THIS DATA: STREAM CUT SHORT
!!  REFUSED-TRUNCATED at carrier 2: the stream ended inside a frame: it
!!  was cut short
!!  The carriers verified before it were signed by
!!  03c1873fcaf504adbbf2746fb36fd8321180c022bd4b20a270726981009911c982.
!!  2097152 bytes of the 2 carrier(s) verified before it are already on
!!  standard output. THE OUTPUT IS INCOMPLETE: do not use it as the whole.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
beefcat: REFUSED-TRUNCATED carrier=2
```

With `-save FILE` in place of the redirect, nothing of a refused stream
is kept.

## 5. SPV fails

The same intact stream, verified by a receiver whose block headers come
from testnet. Every signature is good. The postage's Merkle path does not
lead to a block on that receiver's chain, so nothing is written.

```console
receiver$ beefcat verify -network test -k 03c1873f…9911c982 stream.bcat > out.bin

!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
!!  BEEFCAT REFUSED THIS DATA: FRAUDULENT POSTAGE: NOT PROVEN ON YOUR
!!  CHAIN
!!  REFUSED-BUMP at carrier 0: the funding transaction
!!  f67988abbbd72aa18cbb5d172be280a1b9e568bf933e2e6f277fe32808ef1cce is
!!  not in the chain your header source serves: invalid merkle path for
!!  transaction
!!  f67988abbbd72aa18cbb5d172be280a1b9e568bf933e2e6f277fe32808ef1cce
!!  Nothing was written.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
beefcat: REFUSED-BUMP carrier=0
```

The receiver trusts one thing: the block headers it chose. A proof that
does not reach them is refused, whoever signed it. From the receiver's
side, postage on another chain and forged postage are the same thing.

## 6. The sender retracts

This step was run on a local chain (`cmd/devchain`) between two hosts, so
its transaction ids are not on mainnet. A carrier is the one spender its
funding output is meant to have. The sender spends the outputs itself,
and a receiver that asks the chain about spends then refuses what they
carried.

```console
receiver$ beefcat verify -unspent -k 02c6…9a1e stream.bcat > out.bin
beefcat: VERIFIED 2 carrier(s), 6000000 bytes in 48ms
  retraction  every funding output unspent
```

```console
sender$ beefcat sweep -yes
beefcat: sweep 10690a88…74ebaa99: settling via rpc:http://192.0.2.10:18095/rpc (1160 bytes)
beefcat: sweep 10690a88…74ebaa99: mined at height 803
swept tree e6fd9099…35860cf5 in 10690a88…74ebaa99, mined at height 803: 8 output(s), 4 of them carrying data already sent
```

```console
receiver$ beefcat verify -unspent -k 02c6…9a1e -save kept.bin stream.bcat

!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
!!  BEEFCAT REFUSED THIS DATA: DATA RETRACTED BY ITS SENDER
!!  REFUSED-RETRACTED at carrier 0: funding output
!!  e6fd9099fc978ffad63bd19e7e4d3cd349bd70c84324a626579a0eba35860cf5.2 is
!!  spent by
!!  10690a884f0df1babda87789e8c280bd6d62544fbcec5031528e0dc174ebaa99: the
!!  sender retracted this data
!!  kept.bin was not written: nothing of this stream is kept.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
beefcat: REFUSED-RETRACTED carrier=0
```

Without `-unspent` the same file still verifies. SPV proves a
transaction was mined; it cannot prove an output is still unspent, so a
retraction is seen only by a receiver that looks
([verification.md](verification.md#what-spv-cannot-tell-you)).

## What was not shown

Altered data, a forged sender identity, postage that was never mined,
postage spent twice, a stream reordered or spliced, and a transaction that
could be mined passed off as a carrier are each refused the same way.
They are tests, one case each, in `cmd/beefcat/e2e_test.go`.
