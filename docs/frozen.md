# Frozen

What never changes once a release has used it. Each of these is hashed
into a key, matched byte for byte by a reader, or signed, so changing one
would strand coin or make old streams unreadable. They are defined in
package [`wire`](../wire/wire.go). Anything not listed here is policy and
may change.

## Key derivation

| Item | Value |
| --- | --- |
| BRC-43 protocol | security level 1, name `beefcat carrier` |
| Carrier and funding-output key id | `1`, counterparty anyone |
| Fund key id (the coin pool's address) | `fund`, counterparty self |
| Funding tag, the one field of a funding output | the 12 bytes `beefcat fund` |

A carrier is locked by the key the sender's identity key derives under
the first two rows, so a reader holding only the identity key recomputes
the lock.

## The carrier

Defined by bcommon's `carrier` package and unchanged here:

| Item | Value |
| --- | --- |
| Lock time | 4102444800 (1 January 2100 UTC); a reader refuses anything lower |
| Input sequence | below `0xFFFFFFFF`; a reader refuses a final input |
| Inputs | exactly one, unlocked by one canonical signature, `SIGHASH_ALL\|FORKID` |
| Record output | `<key> OP_CHECKSIG <payload> <signature> OP_2DROP`, the signature over SHA-256 of the payload |
| Funding output | `<key> OP_CHECKSIG <tag> OP_DROP` |

## The payload header

Every carrier's payload opens with 63 bytes, integers big-endian:

| Offset | Size | Field |
| --- | --- | --- |
| 0 | 4 | magic `BCAT` |
| 4 | 1 | version, `1` |
| 5 | 1 | flags: bit 0 set on the last carrier of a stream; the others zero |
| 6 | 16 | stream id, random per stream |
| 22 | 8 | sequence, from 0 |
| 30 | 33 | the sender's identity key, compressed |
| 63 | | data, 0 to 4,194,304 bytes |

A reader refuses an unknown version or a set flag it does not know.

## The stream

| Item | Value |
| --- | --- |
| Preamble | the 8 bytes `BEEFCAT` and `0x01` |
| Frame | a 4-byte big-endian length, then that many bytes: one carrier in Atomic BEEF (BRC-95) with the funding transaction and its Merkle path (BRC-74) |
| Frame length | 1 to 4,259,840 bytes |
| End | the carrier whose last flag is set; nothing follows it |

## The verdict line

Over TCP, after reading the stream to its end, a listener answers with
one line and closes:

```text
beefcat VERIFIED carriers=<n> bytes=<n> age=<n|not-checked> replay=<checked|not-checked>
beefcat <REFUSED-CODE> carrier=<index> <reason>
beefcat ERROR <reason>
```

The first two words are frozen. The rest is for a person.

## Codes and exit statuses

The refusal codes and exit statuses in
[verification.md](verification.md) are what scripts branch on. A later
version may add a code; it does not change the meaning of one.
