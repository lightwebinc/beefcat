# Usage

Every command and flag, the sources beefcat reads, what things cost, and
the limits.

## Commands

```text
beefcat [flags] HOST PORT          send standard input to HOST:PORT
beefcat [flags] -o FILE            write the stream to FILE (- is standard output)
beefcat [flags] -l [ADDR:]PORT     receive one stream, verify it, write it to standard output
beefcat verify [flags] [FILE]      verify a stream from FILE or standard input

beefcat init                       create the sender's home and identity
beefcat fund                       mine postage: funding outputs for carriers
beefcat status                     the home: identity, pool, postage left
beefcat sweep                      spend funding outputs: retract what they carried
beefcat withdraw                   pay coin out of the pool, to an address or an identity key
beefcat version
```

A receiver needs no home, no key and no coin. A sender needs a home
(`init`) and postage (`fund`).

### Sending

| Flag | Default | Meaning |
| --- | --- | --- |
| `-o FILE` | | write the stream to a file instead of a connection; `-` is standard output |
| `-chunk SIZE` | `4M` | data per carrier, at most 4M; bytes, or with `K` or `M` |
| `-lines` | off | one carrier per line of input, sent as each line arrives |
| `-w DURATION` | `2m` | how long to wait for the far end to take a write, and for its verdict |

One carrier spends one funding output. Standard input is cut into chunks
of `-chunk` bytes; the last carrier is signed as the last. With a file on
standard input the size is known, and a send the home's postage cannot
finish is refused before anything is spent. With a pipe, a send that runs
out of postage stops, and the far end refuses the stream as cut short.

`-lines` is for a conversation: each line goes out as soon as it is read
and costs one funding output, and the end of input costs one more for the
empty carrier that closes the stream.

Over TCP the sender waits for the far end's one-line verdict and exits
with it: 0 verified, 3 refused, 4 refused because the far end pinned
another sender. A refusal arrives as soon as the far end reaches it and
stops the send. Carriers are signed a few ahead of the one being written,
one per core and at most 16, so that many more than were sent may have
been spent.

### Receiving

| Flag | Default | Meaning |
| --- | --- | --- |
| `-l [ADDR:]PORT` | | listen for one connection, then exit |
| `-k KEY` | none | the sender's identity key; repeat, or separate several with commas. Any other sender is refused |
| `-unspent` | off | also ask the chain view whether each funding output is spent by another transaction, and refuse data its sender retracted |
| `-save FILE` | | write the data to FILE only if the whole stream verified, instead of standard output; FILE must not exist |
| `-w DURATION` | `2m` | how long a silent connection is kept |

`beefcat verify` takes the same flags and reads a stream from a file or
standard input. What is checked, and how each failure is reported, is in
[verification.md](verification.md).

`-l PORT` listens on every address. Give an address to listen on one:
`-l 127.0.0.1:9000`. A listener takes the first connection and exits
with its verdict, so anything that connects first, a port scanner
included, ends it with a refusal. Listen where only the sender reaches.

### init

```text
beefcat init [-network main|test|regtest] [-headers S] [-chain S] [-settle S] [-devchain URL]
```

Creates the home (mode 0700) with a new identity key and an empty coin
pool, records the network and sources in `config.json`, and prints the
identity key and the fund address. A home that exists keeps its identity;
sources given on the command line are saved over the recorded ones.

The home is `~/.beefcat`, or `BEEFCAT_HOME`, or `-home DIR` on any
command. `identity.json` in it is the private key: whoever holds it can
send as you. Back it up, and keep it private.

### fund

```text
beefcat fund [-n N] [-txid TXID | -beef FILE|-] [-wait DURATION]
```

Makes sure the home has postage for N carriers (default 64). It mints one
funding tree holding the outputs that are missing, broadcasts it, and
waits up to `-wait` (default 30 minutes) for the block that mines it. A
tree holds at most 1,023 outputs; a larger N is minted as several trees,
one block each.

`fund` is a top-up, so it is safe to run again. A run that timed out
waiting for a block left a record of the transaction it broadcast; the
next run asks the chain what became of it before minting anything.

The tree is paid from the home's coin pool. Fill the pool by paying the
fund address from your own wallet, then import that payment:

| Flag | Meaning |
| --- | --- |
| `-txid TXID` | import a mined payment, read from the chain view and checked against the header source |
| `-beef FILE` | import a mined payment as the BEEF your wallet hands over, binary or hex, with no lookup; `-` is standard input |
| `-coinbase` | mine coinbase to the fund address on a local chain. Coinbase: only on a regtest chain you run, for development and tests |

`fund -n 0 -txid TXID` imports a payment and mints nothing.

### status

Prints the home, network, identity key, fund address, sources, pool,
postage left, each funding tree, and totals sent. It reads only.

### sweep

```text
beefcat sweep [-tree TXID] -yes
```

Spends every funding output of the home's trees, or of one tree, in one
mined transaction per tree, and waits for its block. This is how a sender
retracts: once the sweep is mined, a receiver that checks spends
(`-unspent`) refuses every carrier those outputs funded. Outputs not yet
used go with them, so the tree funds nothing more. The sweep's miner fee
comes from the pool. Without `-yes` it prints what it would do and spends
nothing.

A retraction reaches only receivers that look. A receiver that verified
the data before the sweep, or that does not use `-unspent`, still has it.

### withdraw

```text
beefcat withdraw [-sats N] [-messagebox URL] -yes TO
beefcat withdraw -redeliver TXID
```

Pays coin out of the home's pool. Without `-sats` it pays the pool's
largest coin whole, less the miner fee. Funding outputs already minted are
postage, not coin, and are not withdrawn. Without `-yes` it prints what it
would do and spends nothing.

| TO | Payment |
| --- | --- |
| an address | a plain payment to it |
| an identity key (66 hex characters), with `-messagebox URL` | a BRC-29 payment: locked to a key derived for this one payment, which only that identity's wallet can spend |
| `peerpay:KEY?url=URL` | the same, with the message box taken from the link |

A wallet cannot see a BRC-29 payment until it is told the derivation, so
beefcat hands the recipient a PeerPay notice: the payment with its proofs
and the derivation, encrypted to the recipient, put in its
`payment_inbox` at the message box server over an authenticated
connection (BRC-103). A wallet that speaks PeerPay lists it as an
incoming payment.

The notice is written to `payments/<txid>.json` in the home before the
payment is broadcast. If the message box does not take it, beefcat says so
loudly and exits nonzero; the payment is made and nothing is lost.
`-redeliver TXID` sends the kept notice again and spends nothing. A message
box that charges for delivery is refused, not paid.

## Sources

| Setting | Flag | Default on main and test | What it is |
| --- | --- | --- | --- |
| headers | `-headers` | `woc:<network>` | the header source every proof is checked against: `woc:main`, `woc:test`, `chaintracks:URL`, `bhs:URL` (a block-headers-service), `arcade:URL`, or a URL serving `/v1/root/<height>` and `/v1/tip` |
| chain | `-chain` | `woc:<network>` | the chain view, for importing a payment, waiting for a block, and `-unspent`: `woc:main`, `woc:test`, `asset:URL` (a node) |
| settle | `-settle` | `arcade:<network>` | where funding trees and sweeps are broadcast: `arcade:main`, `arcade:test`, `arcade:URL`, `arc:URL`, `rpc:URL` |

A sender's home records them at `init`; a flag overrides one for a run.
A receiver with no home uses the defaults for `-network` (default
`main`), or what its flags say. Network `regtest` has no defaults: name
every source, or use `init -devchain URL`.

The header source is the one thing a receiver trusts. A public service is
a reasonable start; your own block-headers-service or node is better.

### What talks to the network

| Command | Reaches |
| --- | --- |
| send | the far end only |
| receive, `verify` | the far end; the header source, at most once per carrier |
| receive with `-unspent` | also the chain view, once per carrier |
| `fund`, `sweep`, `withdraw` | the header source, the chain view and the settlement leg |
| `withdraw` to an identity key | also the recipient's message box server |
| `init`, `status` | nothing |

## Costs

The sender pays miner fees for postage, once, and nothing per byte sent.
The receiver pays nothing. No payment moves between the two.

A funding tree is one mined transaction of about 190 bytes plus 58 bytes
per output. Each output holds one satoshi. At the network's rate of 100
satoshis per 1,000 bytes, with a 100 satoshi minimum:

| Outputs | Data at 4 MiB a carrier | Transaction | Cost |
| --- | --- | --- | --- |
| 1 | 4 MiB | 249 bytes | 101 satoshis |
| 64 | 256 MiB | 3,904 bytes | 455 satoshis |
| 256 | 1 GiB | 15,042 bytes | 1,761 satoshis |
| 1,023 | 4 GiB | 59,528 bytes | 6,976 satoshis |

Measured on a local chain at that rate. Cost is per carrier, not per
byte: a thousand one-line messages cost as much postage as 4 GiB.

## Limits

| Limit | Value |
| --- | --- |
| Data in one carrier | 0 to 4 MiB (4,194,304 bytes) |
| One frame | 4,259,840 bytes |
| Outputs in one funding tree | 1,023 |
| A stream | any length; 2^64 carriers |
| Connections per listener | one, then it exits |

Each carrier carries the funding tree it spends from, so the bytes on the
wire are the data plus, per carrier, the tree's size and about 430 bytes.
From a 256-output tree that is 0.4 % over the data at 4 MiB a carrier,
and 1.4 % from a 1,023-output tree. For short messages, fund a small tree:
a one-line message from a 1-output tree is about 700 bytes on the wire,
and about 60,000 from a 1,023-output tree.

## Speed

Carriers are signed and verified on several cores at once, one per core
and at most 16, each holding one carrier in memory. Measured with 1 GiB
of random data on a local chain:

| Run | Time | Rate |
| --- | --- | --- |
| Send to a file, 32-thread workstation | 0.9 s | about 1.2 GB/s |
| Verify a file, same machine | 1.0 s | about 1.1 GB/s |
| Send over a 10 GbE link to a 16-thread host, verified there | 1.4 s | about 750 MB/s |

The work is hashing, so a CPU without SHA extensions is several times
slower per core. A receiver's first proof costs one request to its header
source: about 0.3 s against a public service, which is most of the time a
small transfer takes. A header service of your own removes it.

The data is signed, not encrypted, and the connection is plain TCP.
Anyone on the path can read it. Nobody on the path can change it
unnoticed, though they can replay an older stream from the same sender:
see [verification.md](verification.md#what-spv-cannot-tell-you).
