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
| 8 | Same sender and same stream as the first carrier, and the next sequence number | `REFUSED-KEY`, `REFUSED-SEQ` |
| 9 | It spends a beefcat funding output of the sender's own, in a transaction that carries its own Merkle path, and that path leads from that transaction | `REFUSED-DECODE`, `REFUSED-BUMP` |
| 10 | No earlier carrier of the stream spent that output | `REFUSED-FORK` |
| 11 | SPV: the input's signature satisfies the funding output, and the Merkle path leads to a root your header source holds at that height | `REFUSED-SIG`, `REFUSED-BUMP` |
| 12 | With `-max-age` or `-min-conf`: the funding transaction lies no more blocks below your header source's tip than you allow, and has the confirmations you require | `REFUSED-STALE` |
| 13 | With `-unspent`: the chain view shows the funding output unspent, or spent by this carrier | `REFUSED-RETRACTED` |
| 14 | With `-seen`: the funding output is not in your record of outputs you accepted carriers on | `REFUSED-REPLAY`, `REFUSED-FORK` |
| 15 | The sender is one you pinned with `-k`, if you pinned any | `REFUSED-UNTRUSTED` |

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

## Age and replay

A carrier is never mined, so nothing on the chain says an output was
used, and a stream that verified once verifies again. Two checks, both
off unless asked for, close that for one receiver.

**Age** (`-max-age N`, `-min-conf N`). A carrier has no time of its own.
The one time the chain vouches for is the height of the block that mined
its postage, and it is believed only after check 11 placed the funding
transaction in that block. Age is the tip's height less that height, in
blocks. `-max-age 144` refuses postage more than 144 blocks old;
`-max-age 24h` is the same, taken at ten minutes a block. `-min-conf 6`
refuses postage with fewer than six confirmations, counting the block
that mined it as the first, for a receiver that will not rest on a block
a reorganization could still replace. The tip is asked of your header
source once per stream. If it does not answer there is no verdict, exit
1. A proof at a height above the tip the source reports counts as the
tip.

**Replay** (`-seen FILE`). The receiver records each funding output as
it accepts a carrier on it, in FILE, and makes the record durable before
it writes that carrier's data. A carrier on an output already there is
refused: `REFUSED-REPLAY` when it is the carrier recorded, and
`REFUSED-FORK` when it is another, which means the sender signed two
carriers on one output. A crash between the record and the write can
lose data and cannot let a stream in twice. The outputs of a stream
refused part way stay recorded: its sender marked them spent before
signing, and sends again on fresh ones.

The two work together. Alone, the record grows by 72 bytes for every
carrier, forever. With `-max-age`, an output older than the limit is
refused for its age, so its record is dropped when the file is next
opened. The height it was cleared to is kept in the file as a floor that
never falls, and postage mined below the floor is refused as
`REFUSED-STALE` whatever limit a later run gives and whatever tip the
header source reports then: what the record forgot, it does not take
again. The file is cleared against the tip when it is opened and a
stream is measured against the tip when it arrives, which for a listener
may be much later; the floor is what makes a lower tip then harmless.

What they do not do:

| Limit | Why |
| --- | --- |
| Age is the postage's, not the data's | A sender can sign on postage at any time, before it is mined or long after, and inside the limit an old stream is as young as a new one. Only the record refuses it |
| The record is this receiver's | A stream one receiver took still verifies at another whose record is empty. Nothing in a carrier names who it is for |
| The tip comes from your header source | A source that understates its tip makes old postage look young. It is the same single source the proofs rest on: see below |

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
   when, and it can be earlier still: a carrier can be signed on
   postage before that postage is mined. `-max-age` bounds how long ago
   the postage was mined, not when the data was signed.
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
   sender in its place, and it verifies. `-seen FILE` refuses a stream
   this receiver took before, and `-max-age` one on postage older than
   you allow. A stream you never took, on postage still young enough,
   passes both. If freshness matters beyond that, put it in the data: a
   date, a counter, a challenge you chose.
5. **What this message cost.** Postage is a setup cost. Within a stream,
   one funding output backs one carrier, and a second use is refused, so
   a stream needs as many mined outputs of the sender's as it has
   carriers. But a carrier is never mined, so the chain does not record
   an output as used: the same stream can be sent to many receivers, and
   a receiver sees an output that backed an earlier stream only if it
   kept a record of its own (`-seen`), and only of streams it took
   itself. The proof says the sender's key
   paid to exist on the chain, not that it paid for this delivery.
6. **Anything about secrecy.** The data is signed, not encrypted. Anyone
   on the path can read it.

## How far the header check goes

Your header source is the one thing you trust, and today beefcat trusts
it more than SPV has to. For each block a proof names, it asks one remote
source for that block's header and checks two things: that the header's
own proof of work meets its stated target, and, on mainnet, that the
target is no easier than a fixed floor well below real difficulty. It
does not hold a chain of headers, so it does not check that the header
links back to the ones before it, or that it is on the chain with the
most work. A source that serves only Merkle roots, with no header
fields (a bare URL), has nothing checked at all: it is taken at its word.

That is weaker than a real block in two ways:

| Gap | What it allows |
| --- | --- |
| One source | Whoever controls the source, or the path to it, can serve a header of their own |
| No chain linkage, and a floor instead of real difficulty | A forged header needs only the floor's work, once, not the work of a real block and everything after it |

So against a source that serves headers, a forged proof costs an
attacker who can answer for it about as much as mining one header at the
floor. That is real work, and far less than out-working the chain.
Against a source that serves only roots it costs no work at all: use one
only where you run it and trust the path to it.

A local header chain on the receiver closes both gaps: every header
checked for work and linked to its parent, kept from several sources,
with the most-work chain taken. beefcat does not have one yet. Until it
does, point `-headers` at a header service you run (`bhs:URL`,
`chaintracks:URL`), which keeps such a chain itself, and reach it over a
path you trust.

The tip that `-max-age` and `-min-conf` measure against comes from the
same one source and is taken at its word, so a source that understates
its tip passes postage older than your limit, and one that overstates it
refuses postage that is young enough. A local header chain closes that
too.

A header source that fails, rather than answering no, gives no verdict.
A proof naming a block the source will not answer for at all can
therefore end as exit 1 instead of a refusal. Either way nothing is
written.
