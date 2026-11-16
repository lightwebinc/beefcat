# Verification

What the far end checks before it writes a byte, what each check proves,
how a failure is reported, and what none of it can tell you.

## The three questions

Someone hands you bytes and says who sent them. beefcat answers three
questions without trusting the connection, the network in between, or
whoever handed the bytes over:

| Question | How it is answered |
| --- | --- |
| Are these the bytes that were sent? | Each carrier's transaction id is a hash of its bytes, and a signature covers the data inside it |
| Who signed them? | The carrier is locked to a key derived from the identity key it names, and signed under that key |
| Did that sender pay to send them? | The carrier spends an output of a transaction whose Merkle path leads to a block header you hold |

The first two are arithmetic on the bytes received. Only the third asks
anything of the outside: your header source, for the Merkle root of one
block.

## The checks, in order

A stream is a sequence of carriers. For each one, in this order, and
stopping at the first that fails:

| # | Check | Refused as |
| --- | --- | --- |
| 1 | The frame is within bounds, is Atomic BEEF, holds no more than a carrier and what it spends, and parses to the transaction it names | `REFUSED-DECODE` |
| 2 | The carrier comes with no Merkle path of its own: a carrier is never mined | `REFUSED-BUMP` |
| 3 | The transaction is a carrier with exactly one beefcat record, with a well-formed header | `REFUSED-DECODE` |
| 4 | The transaction cannot be mined: lock time 1 January 2100, input not final | `REFUSED-MINEABLE` |
| 5 | Its input is unlocked by exactly one canonical signature, so nobody can rewrite it under another transaction id | `REFUSED-UNLOCKING` |
| 6 | Its lock is the key the named identity derives | `REFUSED-KEY-DERIVE` |
| 7 | The signature over the data verifies under that key | `REFUSED-SIG` |
| 8 | Same sender and same stream as the first carrier, the next sequence number, naming the transaction id of the carrier before it | `REFUSED-KEY`, `REFUSED-SEQ` |
| 9 | It spends a beefcat funding output of the sender's own, in a transaction that carries its own Merkle path, and that path leads from that transaction | `REFUSED-DECODE`, `REFUSED-BUMP` |
| 10 | No earlier carrier of the stream spent that output | `REFUSED-FORK` |
| 11 | SPV: the input's signature satisfies the funding output, and the Merkle path leads to a root your header source holds at that height | `REFUSED-SIG`, `REFUSED-BUMP` |
| 12 | With `-unspent`: the chain view shows the funding output unspent, or spent by this carrier | `REFUSED-RETRACTED` |
| 13 | The sender is one you pinned with `-k`, if you pinned any | `REFUSED-UNTRUSTED` |

Only then is the carrier's data written. After the carrier that signs
itself as the last:

| Check | Refused as |
| --- | --- |
| The stream ended before a signed last carrier | `REFUSED-TRUNCATED` |
| Anything follows the signed last carrier | `REFUSED-SEQ` |

Check 8 and the two above are what a plain pipe cannot give you. The
order of the data and its end are signed, so a stream that was cut,
shuffled, or spliced with carriers from another stream by the same
sender does not verify, and neither does one with a carrier repeated.

The sender is asked about last, so `REFUSED-UNTRUSTED` always means the
same thing: the data is genuine, and it is not from a sender you pinned.
Data that fails any other check is refused for that, whoever sent it.

Check 9 closes a cheap forgery. A transaction that spends a mined coin
verifies by SPV through that coin even if it was never broadcast, so a
sender could sign postage and never pay for it. beefcat requires the
funding transaction itself to be in a block.

## How a failure is reported

A refusal is a verdict: the data should not be believed. It is printed in
a banner on standard error, with a bell and color on a terminal, and
never silenced by `-q`:

```text
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
!!  BEEFCAT REFUSED THIS DATA: FORGED OR ALTERED DATA
!!  REFUSED-SIG at carrier 1: carrier: field signature does not verify: the
!!  data is not what the sender signed
!!  The carriers verified before it were signed by 02c6...9a1e.
!!  1000 bytes of the 1 carrier(s) verified before it are already on
!!  standard output. THE OUTPUT IS INCOMPLETE: do not use it as the whole.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
beefcat: REFUSED-SIG carrier=1
```

The last line is for scripts: the code, and the index of the carrier
refused, from 0.

| Exit | Meaning |
| --- | --- |
| 0 | The whole stream verified |
| 1 | No verdict: the header source or chain view did not answer, or reading or writing failed |
| 2 | Usage |
| 3 | Refused: the data did not verify |
| 4 | Refused: the data verified, but its sender is not one you pinned |

An outage is never reported as a forgery, and a forgery is never
reported as an outage. Exit 1 says beefcat could not decide.

Over TCP the listener answers the sender with its verdict in one line,
and the sender prints it and exits with the same status. That line is a
courtesy from the far end, not a proof of anything.

### What reaches standard output

A carrier's data is written only after that carrier passed every check,
so nothing unverified is ever written. A stream that fails part way
leaves the data of the carriers before it on standard output: verified,
but incomplete, and the banner says so. To keep nothing of a stream that
did not verify to its end, receive with `-save FILE`: the file appears
only when the whole stream verified. `-save` does not overwrite: a file
left by an earlier run is refused before anything is read.

## Pinning the sender

Verification proves that some key signed the data and paid for it. It
cannot know whose data you were expecting. Without `-k`, beefcat writes
data from any sender whose carriers verify, and says so in a warning as
loud as a refusal, naming the key so you can pin it next time.

With `-k KEY` (repeat it, or separate keys with commas), data from any
other sender is refused before a byte is written, with exit 4. The key is
the sender's identity key, which `beefcat init` and `beefcat status`
print. Get it from the sender over a channel you already trust.

## What SPV cannot tell you

1. **When the data was made.** The postage has a block height. The data
   was signed at some time after the postage was created; nothing says
   when.
2. **That the data is still current.** SPV proves a transaction was
   mined. It cannot prove an output is still unspent. A sender retracts
   by spending the funding outputs (`beefcat sweep`), and only a view of
   the chain's spends can see that. `-unspent` asks one, and trusts its
   answer: there is no proof of absence.
3. **That the sender is who you think.** A key is a key. Whose it is
   comes from how you learned it.
4. **That this is the stream just sent.** A verified stream proves the
   sender signed it at some time. Someone on the path cannot change a
   stream, but can hand you an older complete stream from the same
   sender in its place, and it verifies. If freshness matters, put it in
   the data: a date, a counter, a challenge you chose.
5. **That the postage was not used before.** Within a stream, one
   funding output pays for one carrier, and a second use is refused. A
   receiver keeps no memory between streams, so it cannot see an output
   used again in a later stream. Each stream still needs as many mined
   outputs of the sender's as it has carriers.
6. **Anything about secrecy.** The data is signed, not encrypted. Anyone
   on the path can read it.

Your header source is the one thing you trust. A source that serves
header fields (WhatsOnChain, a block-headers-service, chaintracks,
arcade) has each header's proof of work checked, on mainnet against a
difficulty floor. Run your own where it matters.

A header source that fails, rather than answering no, gives no verdict.
A proof naming a block the source will not answer for at all can
therefore end as exit 1 instead of a refusal. Either way nothing is
written.
