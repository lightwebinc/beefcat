# Design

Why beefcat is built the way it is, what it leaves out on purpose, and
where it can grow.

## What it demonstrates

Data can be proven without being mined. A transaction that is never mined
is still verifiable when it spends an output of one that was, and a
transaction with a lock time of 1 January 2100 and a non-final input
cannot be mined before then by any node. Put data in such a transaction
and you have a **carrier**: provable against block headers, free of block
space, and retractable by its sender with one mined spend.

beefcat is the smallest whole use of that idea: no host, no overlay, no
index, no account. Two programs and a TCP connection.

## Nearest things

| Thing | What it does | What beefcat adds or leaves out |
| --- | --- | --- |
| netcat | moves bytes between two endpoints | proof of what the bytes are, who signed them, and that sending them cost something; a signed order and a signed end |
| A detached signature (a signed checksum beside a file) | proves who signed the bytes | postage: an identity that costs nothing to make can sign anything, while a carrier's key has a mined cost a reader verifies from headers alone; and retraction |
| BEEF (BRC-62, BRC-95) | carries a payment with its proofs from payer to payee | the same envelope, carrying data instead of value |
| [bstore](https://github.com/lightwebinc/bstore) | stores objects with providers, on the same carriers | none of the market: one sender, one receiver, nothing kept |
| BRC-141 | moves arbitrary payloads across an air gap, with a checksum | a different transport; it says nothing of who sent a payload |

## Who pays

| | Pays | For |
| --- | --- | --- |
| Sender | miner fees, once, for a funding tree | one funding output per carrier, however full |
| Receiver | nothing | |
| Between them | nothing moves | |

No payment travels from sender to receiver or back, and beefcat says so
plainly: it demonstrates the carrier, not a payment flow. The seam is
there. A receiver learns the sender's identity key from a verified
stream, and an identity key is payable with no further exchange (BRC-29
derives a payment destination from it).

Postage puts a mined cost behind every sender, and a receiver checks it
from headers alone: a stream of n carriers needs n mined outputs of the
sender's own. It is a setup cost, not a cost per message or per receiver.
Carriers are never mined, so the chain does not record an output as
used: one stream can go to many receivers for the postage of one.

A receiver can ask for more. With `-seen` it keeps its own record of the
outputs it accepted carriers on and takes each once. With `-max-age` it
takes only postage mined within so many blocks, and then postage is
perishable: a sender who keeps sending to that receiver mints a fresh
tree every window, and the cost recurs with the window instead of being
paid once. Neither reaches past the one receiver. A stream it took still
verifies at another receiver, because nothing in a carrier names who it
is for; an index shared between receivers is a host's job and a seam
here.

## Shape

| Question | Answer |
| --- | --- |
| Where is the state? | The sender's home: an identity key, a coin pool, and which funding outputs are spent. The receiver keeps none, unless it asks for a record of outputs seen (`-seen`): one file, held by one receiver at a time |
| What is the serialization point? | One funding output per carrier, per sender. Two senders share nothing |
| What is stateless? | The transport, and a receiver without `-seen` |
| What runs in parallel? | Any number of streams, senders and receivers |

A funding output is marked spent in the sender's state before its carrier
is signed, so an interrupted send never signs two carriers on one output.

## Choices

**Verify before write, carrier by carrier.** A receiver writes a
carrier's data only after every check on it passed, so standard output
never holds an unverified byte. The cost is that a stream which fails
part way leaves a verified prefix behind; `-save` trades streaming for
all-or-nothing.

**The order and the end are signed.** Each carrier names its stream and
its sequence number, the last one says it is the last, and the sender
signs that with the data. A transport that drops, repeats, reorders or
truncates is caught by arithmetic, not by trusting TCP.

**No carrier depends on another's bytes.** A carrier could also name the
transaction id of the one before it. That adds nothing a third party
could not already be caught at, since every carrier is signed over its
place in the stream, and it would force the sender to sign one carrier at
a time. Without it a sender signs, and a receiver checks, as many
carriers at once as it has cores; only the order is settled in sequence.

**Postage must itself be mined.** A funding transaction that merely
spends a mined coin verifies by SPV through its parent without ever being
broadcast. beefcat refuses it: the funding transaction carries its own
Merkle path or the carrier is refused.

**Loud by default.** A refusal, an unpinned sender and an undecided
verification each print a banner that `-q` does not silence, and each has
its own exit status. A tool whose point is to say when data should not be
believed must not be easy to ignore.

**Age is the postage's block height, and replay is the receiver's own
record.** A carrier carries no clock a receiver could trust, so the only
age checked is the one the chain vouches for. A replay is caught by what
the receiver wrote down itself, before it wrote the data, and the age
limit is what lets that record forget. Both are off by default: a
receiver with neither is the stateless one the rest of this page
describes.

**Sending asks nothing of the chain.** Postage is mined ahead and kept
with its proof, so a send works wherever a TCP connection does, and its
speed is the speed of hashing and signing.

**The sender is pinned by key, not by name.** Resolving a name to a key
is a different problem with its own answers
([bfinger](https://github.com/lightwebinc/bfinger)); beefcat takes the
key.

## Not built

Each of these is a seam, left open rather than closed off.

| Not built | What it would open |
| --- | --- |
| Multicast delivery | The frame is already the object a multicast plane carries: one send reaching every subscribed receiver, each verifying on its own |
| A payment back to the sender | A receiver paying for what it verified, from the identity key it just proved |
| Encryption | Confidential streams; today the data is signed and readable on the path |
| Resolving a name to the sender's key | `-k` taking a handle instead of a key |
| Keeping a listener open for many streams | A drop box |
| A receiver that watches for retraction after the fact | Today `-unspent` looks once, at verification time |
| A record of outputs seen that receivers share | Refusing a stream another receiver already took; today each `-seen` file is one receiver's |
| A carrier that names who it is for | Refusing a stream sent to someone else, with no shared record |
| A local header chain on the receiver | Headers checked for work and linked to their parents, from several sources: a forged proof would have to out-work the chain, not one header at a floor from one source |
