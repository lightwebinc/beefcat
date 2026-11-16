# beefcat

[![CI](https://github.com/lightwebinc/beefcat/actions/workflows/ci.yml/badge.svg)](https://github.com/lightwebinc/beefcat/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/lightwebinc/beefcat)](https://github.com/lightwebinc/beefcat/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/lightwebinc/beefcat.svg)](https://pkg.go.dev/github.com/lightwebinc/beefcat)
[![Go version](https://img.shields.io/github/go-mod/go-version/lightwebinc/beefcat)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

> [!WARNING]
> **Experimental software.** beefcat builds on [bstack](https://github.com/lightwebinc/bstack). It is published to be built on and improved. Interfaces, formats and behavior may change rapidly between releases; pin an exact version.

netcat for the BEEF data carrier, with SPV.

Any bytes in, sent to an address and port, and the far end proves three
things about them before it writes one: the bytes are exactly what was
sent, a named key signed them, and that key holds mined postage on the BSV
blockchain. The proof travels with the data. The receiver
needs nothing but block headers, from a source it chooses, and the proof
is as strong as that source: see
[how far the header check goes](docs/verification.md#how-far-the-header-check-goes).

```console
receiver$ beefcat -l 9000 -k 03c1873f...9911c982 > received.bin
sender$   beefcat receiver.example.com 9000 < file.bin
```

```text
beefcat: VERIFIED 5 carrier(s), 20000000 bytes in 596ms
  sender      03c1873f...9911c982 (pinned with -k)
  postage     funding transaction f67988ab...08ef1cce, mined at height 975787
  headers     woc:main
```

That is a real run on mainnet; [docs/walkthrough.md](docs/walkthrough.md)
has all of it.

The data is never mined. Each chunk rides a **carrier**: a signed
transaction built so that it cannot be mined, spending one output of a
transaction that was. That mined output is the postage; the carrier and
its Merkle path, in one [BEEF](https://github.com/bsv-blockchain/BRCs/blob/master/transactions/0062.md)
envelope, are the proof.

Postage is a setup cost, not a fee per message. beefcat spends one funded
output per carrier and never spends one twice, and enough of them for a
gigabyte costs about 1,800 satoshis in miner fees. But a carrier is never
mined, so nothing on the chain records that an output was used: the same
stream can go to any number of receivers at no further cost, and a
receiver, which keeps no memory, cannot tell an output that backed an
earlier stream. What the proof shows is that the sender's key paid to
exist on the chain, not what any one message cost.

## It is loud when it should be

beefcat exists to tell you when data should not be believed, so it does
not whisper. Each of these stops the stream, writes nothing of the
carrier that failed, prints a banner, and exits nonzero:

| What happened | Code | Exit |
| --- | --- | --- |
| Data altered, or signed by someone else | `REFUSED-SIG`, `REFUSED-DECODE` | 3 |
| A sender claiming a key that is not theirs | `REFUSED-KEY-DERIVE` | 3 |
| Postage not on your chain, never mined, or spent twice | `REFUSED-BUMP`, `REFUSED-FORK` | 3 |
| Stream cut short, reordered, spliced or appended to | `REFUSED-TRUNCATED`, `REFUSED-SEQ` | 3 |
| A transaction that could be mined, passed off as a carrier | `REFUSED-MINEABLE` | 3 |
| Data its sender retracted (with `-unspent`) | `REFUSED-RETRACTED` | 3 |
| Verified, but not from a sender you pinned | `REFUSED-UNTRUSTED` | 4 |
| The header source did not answer: no verdict | `ERROR` | 1 |

A stream that verifies from a sender you did not pin is written, with a
warning as loud as a refusal: anyone with a funded key can send data that
verifies, so pin the key you expect with `-k`.

## Start here

| To | Read |
| --- | --- |
| Try it in two minutes on one machine, with no coin | [QUICKSTART.md](QUICKSTART.md) |
| Send on mainnet | [QUICKSTART.md](QUICKSTART.md#on-mainnet) |
| See a real run on mainnet, with every refusal | [docs/walkthrough.md](docs/walkthrough.md) |
| See it on the wire: a packet capture with every byte named | [docs/capture.md](docs/capture.md) |
| Every command and flag, costs and limits | [docs/usage.md](docs/usage.md) |
| What is checked, in what order, and what SPV cannot tell you | [docs/verification.md](docs/verification.md) |
| The formats that never change | [docs/frozen.md](docs/frozen.md) |
| Why it is built this way | [docs/design.md](docs/design.md) |

## Install

```console
$ go install github.com/lightwebinc/beefcat/cmd/beefcat@latest
```

or take a release tarball. A receiver needs only the binary. A sender
also needs a home (`beefcat init`) and postage (`beefcat fund`).

## License

Apache-2.0: see [LICENSE](LICENSE), [NOTICE](NOTICE) and
[LICENSE-THIRD-PARTY](LICENSE-THIRD-PARTY).
