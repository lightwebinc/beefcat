# On the wire

One short message sent with beefcat on BSV mainnet postage, captured with
tcpdump, and every byte of it named. The capture is in this directory and
opens in Wireshark:

| File | What it is |
| --- | --- |
| [capture/beefcat-mainnet-hello.pcap](capture/beefcat-mainnet-hello.pcap) | the raw capture: 11 packets on the loopback interface |
| [capture/message.txt](capture/message.txt) | the 134 bytes that were sent |
| [capture/decode.py](capture/decode.py) | the decoder that produced the tables below; it needs only Python 3 |

Sender and receiver ran on one host, so the addresses are loopback. The
postage is real: the funding transaction is
[`f67988abbbd72aa18cbb5d172be280a1b9e568bf933e2e6f277fe32808ef1cce`](https://whatsonchain.com/tx/f67988abbbd72aa18cbb5d172be280a1b9e568bf933e2e6f277fe32808ef1cce), mined in block 975,787 on mainnet. The
carrier, `eb04760af99b4dd2bb56375084381a6ab7347b6452639bde0dcbaf184cc467de`, is on no chain and never will be.

Wireshark has no dissector for the stream, so it shows the payload as TCP
data; "Follow TCP Stream" shows the same bytes as the tables here.

## The conversation

The whole stream, 4,755 bytes, fits in one segment. The receiver then
verifies, which is arithmetic plus one request to its own header source
for one block's Merkle root, and answers with one line.

| ms | Direction | Flags | Payload | What is happening |
| --- | --- | --- | --- | --- |
| 0.00 | sender → receiver | SYN |  | Sender opens the connection |
| 0.01 | receiver → sender | SYN ACK |  | Receiver accepts |
| 0.03 | sender → receiver | ACK |  | Handshake complete |
| 14.42 | sender → receiver | PSH ACK | 4,755 bytes | The whole stream: preamble, frame length, one carrier in Atomic BEEF |
| 14.44 | receiver → sender | ACK |  | Acknowledged |
| 14.50 | sender → receiver | FIN ACK |  | Sender half-closes and waits for the verdict |
| 55.58 | receiver → sender | ACK |  | Acknowledged |
| | | | | *389 ms: the receiver verifies, and asks its header source for the root of block 975,787* |
| 444.59 | receiver → sender | PSH ACK | 38 bytes | The verdict: `beefcat VERIFIED carriers=1 bytes=134` |
| 444.60 | sender → receiver | ACK |  | Acknowledged |
| 444.62 | receiver → sender | FIN ACK |  | Receiver closes |
| 444.63 | sender → receiver | ACK |  | Acknowledged |

## What the bytes are for

Every byte has one of six roles.

| Role | Bytes | What it covers |
| --- | --- | --- |
| Delivery | 12 | framing: the preamble and the frame length |
| Proof | 436 | the envelope, the Merkle path, and the links SPV follows |
| Mined | 3,762 | the funding transaction, which is in a block |
| Carrier | 59 | the transaction that is never mined, and its signed header |
| Keys and signatures | 352 | public keys and signatures: who signed, and under which key |
| Data | 134 | the sender's bytes |

Of 4,755 bytes, 134 are the message. Most of the rest is the
funding transaction, which holds 64 funding outputs and travels with every
carrier that spends one of them; a sender with short messages funds a
smaller tree (see [usage.md](usage.md#limits)).

## Every field

Offsets are from the first byte the sender wrote. Long values are
shortened in the middle; the capture has them whole.

### Stream

| Offset | Bytes | Role | Field | Value | Meaning |
| --- | --- | --- | --- | --- | --- |
| 0 | 8 | Delivery | Preamble | `BEEFCAT + 0x01` | Opens every stream: the letters BEEFCAT and the framing version. |
| 8 | 4 | Delivery | Frame length | `4,743 bytes` | Big-endian byte count of the one frame that follows. One frame is one carrier. |

### Atomic BEEF envelope

| Offset | Bytes | Role | Field | Value | Meaning |
| --- | --- | --- | --- | --- | --- |
| 12 | 4 | Proof | Atomic BEEF marker | `0x01010101` | BRC-95: this envelope proves exactly one subject transaction. |
| 16 | 32 | Proof | Subject txid | `eb04760af99b4dd2bb56…9bde0dcbaf184cc467de` | The carrier this envelope is about. A receiver refuses the frame if these are not the bytes of that transaction. |
| 48 | 4 | Proof | BEEF version | `0xEFBE0002` | BRC-96, BEEF V2. |

### Merkle path (BUMP)

| Offset | Bytes | Role | Field | Value | Meaning |
| --- | --- | --- | --- | --- | --- |
| 52 | 1 | Proof | Path count | `1` | One Merkle path: the funding transaction's. |
| 53 | 5 | Proof | Block height | `975787` | The block the funding transaction was mined in. The receiver asks its own header source for this block's Merkle root. |
| 58 | 1 | Proof | Tree height | `9` | Levels from the transaction up to the root. |
| 59 | 1 | Proof | Level 0: leaf count | `2` |  |
| 60 | 1 | Proof | Level 0: offset | `174` | Position of this hash in its level of the block's Merkle tree. |
| 61 | 1 | Proof | Level 0: flags | `0` | 0 = a sibling hash follows, 1 = duplicate the working hash, 2 = this is the transaction's own id. |
| 62 | 32 | Proof | Level 0: hash | `b7359467505a71abc16e…32da86530c8b67edcf42` | A sibling hash. Hashed with the working hash to climb one level. |
| 94 | 1 | Proof | Level 0: offset | `175` | Position of this hash in its level of the block's Merkle tree. |
| 95 | 1 | Proof | Level 0: flags | `2` | 0 = a sibling hash follows, 1 = duplicate the working hash, 2 = this is the transaction's own id. |
| 96 | 32 | Proof | Level 0: hash | `f67988abbbd72aa18cbb…2e6f277fe32808ef1cce` | The funding transaction's id. |
| 128 | 1 | Proof | Level 1: leaf count | `1` |  |
| 129 | 1 | Proof | Level 1: offset | `86` | Position of this hash in its level of the block's Merkle tree. |
| 130 | 1 | Proof | Level 1: flags | `0` | 0 = a sibling hash follows, 1 = duplicate the working hash, 2 = this is the transaction's own id. |
| 131 | 32 | Proof | Level 1: hash | `cb8395453a8392b400df…794054edac96391e0905` | A sibling hash. Hashed with the working hash to climb one level. |
| 163 | 1 | Proof | Level 2: leaf count | `1` |  |
| 164 | 1 | Proof | Level 2: offset | `42` | Position of this hash in its level of the block's Merkle tree. |
| 165 | 1 | Proof | Level 2: flags | `0` | 0 = a sibling hash follows, 1 = duplicate the working hash, 2 = this is the transaction's own id. |
| 166 | 32 | Proof | Level 2: hash | `9373fd6362ab12a6545d…e9b162771711199e8af0` | A sibling hash. Hashed with the working hash to climb one level. |
| 198 | 1 | Proof | Level 3: leaf count | `1` |  |
| 199 | 1 | Proof | Level 3: offset | `20` | Position of this hash in its level of the block's Merkle tree. |
| 200 | 1 | Proof | Level 3: flags | `0` | 0 = a sibling hash follows, 1 = duplicate the working hash, 2 = this is the transaction's own id. |
| 201 | 32 | Proof | Level 3: hash | `8da77d44ea5ffb8e2b87…e272fea7a25a2641cc41` | A sibling hash. Hashed with the working hash to climb one level. |
| 233 | 1 | Proof | Level 4: leaf count | `1` |  |
| 234 | 1 | Proof | Level 4: offset | `11` | Position of this hash in its level of the block's Merkle tree. |
| 235 | 1 | Proof | Level 4: flags | `0` | 0 = a sibling hash follows, 1 = duplicate the working hash, 2 = this is the transaction's own id. |
| 236 | 32 | Proof | Level 4: hash | `5a7204736441bccb6cb3…13b4604c5c7168137425` | A sibling hash. Hashed with the working hash to climb one level. |
| 268 | 1 | Proof | Level 5: leaf count | `1` |  |
| 269 | 1 | Proof | Level 5: offset | `4` | Position of this hash in its level of the block's Merkle tree. |
| 270 | 1 | Proof | Level 5: flags | `0` | 0 = a sibling hash follows, 1 = duplicate the working hash, 2 = this is the transaction's own id. |
| 271 | 32 | Proof | Level 5: hash | `708514b6b4dcd858e8e4…cd1ccd13d3625b846423` | A sibling hash. Hashed with the working hash to climb one level. |
| 303 | 1 | Proof | Level 6: leaf count | `1` |  |
| 304 | 1 | Proof | Level 6: offset | `3` | Position of this hash in its level of the block's Merkle tree. |
| 305 | 1 | Proof | Level 6: flags | `0` | 0 = a sibling hash follows, 1 = duplicate the working hash, 2 = this is the transaction's own id. |
| 306 | 32 | Proof | Level 6: hash | `16fe43f42dc07492004d…0c53a9c57caea449eb02` | A sibling hash. Hashed with the working hash to climb one level. |
| 338 | 1 | Proof | Level 7: leaf count | `1` |  |
| 339 | 1 | Proof | Level 7: offset | `0` | Position of this hash in its level of the block's Merkle tree. |
| 340 | 1 | Proof | Level 7: flags | `0` | 0 = a sibling hash follows, 1 = duplicate the working hash, 2 = this is the transaction's own id. |
| 341 | 32 | Proof | Level 7: hash | `bb0d3fb1069dced2424f…e8181f844bfac3a7f9c2` | A sibling hash. Hashed with the working hash to climb one level. |
| 373 | 1 | Proof | Level 8: leaf count | `1` |  |
| 374 | 1 | Proof | Level 8: offset | `1` | Position of this hash in its level of the block's Merkle tree. |
| 375 | 1 | Proof | Level 8: flags | `0` | 0 = a sibling hash follows, 1 = duplicate the working hash, 2 = this is the transaction's own id. |
| 376 | 32 | Proof | Level 8: hash | `c1b1bbb08ab43ba7297e…027c936e998f1266dc6c` | A sibling hash. Hashed with the working hash to climb one level. |

### Transactions

| Offset | Bytes | Role | Field | Value | Meaning |
| --- | --- | --- | --- | --- | --- |
| 408 | 1 | Proof | Transaction count | `2` | Two: the mined funding transaction, then the carrier that spends it. |

### Funding transaction (mined)

| Offset | Bytes | Role | Field | Value | Meaning |
| --- | --- | --- | --- | --- | --- |
| 409 | 1 | Proof | Entry format | `1` | 1 = a raw transaction with a Merkle path. |
| 410 | 1 | Proof | Path index | `0` | Which Merkle path above proves this transaction. |
| 411 | 4 | Mined | Version | `1` |  |
| 415 | 1 | Mined | Input count | `1` |  |
| 416 | 32 | Mined | Input: previous txid | `1d262ba69a96d8666ebd…37c2a4860cdfb55bc484` | The coin that paid for the funding tree. |
| 448 | 4 | Mined | Input: output index | `6` |  |
| 452 | 1 | Keys and signatures | Unlocking script length | `106` |  |
| 453 | 106 | Keys and signatures | Unlocking script | `106 bytes` | Signature and public key that spent the payment into this tree. |
| 559 | 4 | Mined | Input: sequence | `0xffffffff` | Final. |
| 563 | 1 | Mined | Output count | `65` | 64 funding outputs and one change output. |
| 564 | 1,334 | Mined | Funding outputs 0 to 22 | `23 outputs, 1,334 bytes` | Each is one satoshi locked to the sender's key with the tag "beefcat fund". Each pays for one carrier. |
| 1898 | 8 | Mined | Funding output 23: value | `1 satoshi` | One satoshi. This is the output the carrier spends. |
| 1906 | 1 | Mined | Funding output 23: script length | `49` |  |
| 1907 | 1 | Keys and signatures | Push opcode | `push 33 bytes` |  |
| 1908 | 33 | Keys and signatures | Sender's derived key | `03550457609cffcd34db…3cd89302d9019748ec02` | The key the sender's identity key derives. The carrier is locked by the same key, which is how a receiver ties both to one sender. |
| 1941 | 1 | Mined | OP_CHECKSIG | `0xac` | Spendable by one signature from that key. |
| 1942 | 1 | Mined | Push opcode | `push 12 bytes` |  |
| 1943 | 12 | Mined | Funding tag | `"beefcat fund"` | Marks the output as postage. |
| 1955 | 1 | Mined | OP_DROP | `0x75` |  |
| 1956 | 2,354 | Mined | Funding outputs 24 to 63, and change | `41 outputs, 2,354 bytes` | The rest of the postage, unspent, and the change returned to the sender's pool. |
| 4310 | 4 | Mined | Lock time | `0` | None. |

### Carrier (never mined)

| Offset | Bytes | Role | Field | Value | Meaning |
| --- | --- | --- | --- | --- | --- |
| 4314 | 1 | Proof | Entry format | `0` | 0 = a raw transaction with no proof of its own. A carrier never has one. |
| 4315 | 4 | Carrier | Version | `1` |  |
| 4319 | 1 | Carrier | Input count | `1` |  |
| 4320 | 32 | Proof | Input: previous txid | `f67988abbbd72aa18cbb…2e6f277fe32808ef1cce` | The funding transaction. This is the link SPV follows: the carrier is never mined, but it spends an output of a transaction that was. |
| 4352 | 4 | Proof | Input: output index | `23` | Which funding output this carrier spends. One output pays for one carrier. |
| 4356 | 1 | Keys and signatures | Unlocking script length | `72` |  |
| 4357 | 1 | Keys and signatures | Push opcode | `push 71 bytes` | One push, and nothing else: a receiver refuses any other unlocking script, so nobody can rewrite the carrier under another id. |
| 4358 | 70 | Keys and signatures | Input signature (DER) | `70 bytes` | The sender's ECDSA signature over the whole carrier, including its data. It satisfies the funding output's lock. |
| 4428 | 1 | Keys and signatures | Sighash type | `0x41` | SIGHASH_ALL | FORKID: the signature covers every input and output. |
| 4429 | 4 | Carrier | Input: sequence | `0x00000000` | Zero, not final. With the lock time below, this keeps the carrier out of every block. |
| 4433 | 1 | Carrier | Output count | `1` | One output: the record. |
| 4434 | 8 | Carrier | Output value | `1 satoshi` | The funding output's satoshi, carried forward. The carrier pays no fee, because it is never mined. |
| 4442 | 3 | Carrier | Locking script length | `306` |  |
| 4445 | 1 | Keys and signatures | Push opcode | `push 33 bytes` |  |
| 4446 | 33 | Keys and signatures | Sender's derived key | `03550457609cffcd34db…3cd89302d9019748ec02` | Derived from the identity key named in the payload. A receiver recomputes it from that identity and refuses a mismatch. |
| 4479 | 1 | Carrier | OP_CHECKSIG | `0xac` |  |
| 4480 | 1 | Carrier | OP_PUSHDATA1 |  |  |
| 4481 | 1 | Carrier | Payload length | `197 bytes` |  |

### Signed payload

| Offset | Bytes | Role | Field | Value | Meaning |
| --- | --- | --- | --- | --- | --- |
| 4482 | 4 | Carrier | Magic | `"BCAT"` | Marks a beefcat record. |
| 4486 | 1 | Carrier | Version | `1` |  |
| 4487 | 1 | Carrier | Flags | `0x01 (last)` | Bit 0 set: this is the last carrier of the stream. The end is signed, so a stream cut short is refused. |
| 4488 | 16 | Carrier | Stream id | `b7ea21c5fde9df6113e21d3a2c3612c3` | Random per stream. Carriers of another stream cannot be spliced in. |
| 4504 | 8 | Carrier | Sequence | `0` | Position in the stream, from 0. Order is signed. |
| 4512 | 33 | Keys and signatures | Sender's identity key | `03c1873fcaf504adbbf2…a270726981009911c982` | Who claims to have sent this. The receiver pins this key with -k. |
| 4545 | 134 | Data | Data | `Hello from beefcat. …re you read them. ⏎ ` | The sender's bytes, exactly as given. Written out only after every check has passed. |

### Carrier (never mined)

| Offset | Bytes | Role | Field | Value | Meaning |
| --- | --- | --- | --- | --- | --- |
| 4679 | 1 | Keys and signatures | Push opcode | `push 70 bytes` |  |
| 4680 | 70 | Keys and signatures | Field signature (DER) | `70 bytes` | The sender's signature over SHA-256 of the payload: the data is the key holder's, whatever happens to the spend. |
| 4750 | 1 | Carrier | OP_2DROP | `0x6d` |  |
| 4751 | 4 | Carrier | Lock time | `4,102,444,800 (2100-01-01 UTC)` | 1 January 2100. No node may mine this before then: a consensus rule, not a policy. |

## What the receiver did with them

| Check | Fields it reads | Refused as |
| --- | --- | --- |
| The frame is Atomic BEEF and holds the transaction it names | Atomic BEEF marker, Subject txid, the carrier's bytes | `REFUSED-DECODE` |
| The carrier cannot be mined | Lock time, Input: sequence | `REFUSED-MINEABLE` |
| Its input is one canonical signature | Push opcode, Input signature, Sighash type | `REFUSED-UNLOCKING` |
| It is locked by the key the named identity derives | Sender's identity key, Sender's derived key | `REFUSED-KEY-DERIVE` |
| The data is what the sender signed | Field signature, the whole payload | `REFUSED-SIG` |
| It is the next carrier of this stream, and the last is marked | Stream id, Sequence, Flags | `REFUSED-SEQ`, `REFUSED-TRUNCATED` |
| It spends a funding output of the same sender's | Input: previous txid, Input: output index, Funding output 23 | `REFUSED-DECODE` |
| The funding transaction is in a block the receiver's own headers hold | Merkle path, Block height | `REFUSED-BUMP` |
| The sender is the one the receiver pinned | Sender's identity key | `REFUSED-UNTRUSTED` |

All but the header lookup are arithmetic on the bytes above.
[verification.md](verification.md) has the checks in full.

## Decode it yourself

```console
$ python3 docs/capture/decode.py docs/capture/beefcat-mainnet-hello.pcap fields.json
```

The decoder walks the stream by the formats in [frozen.md](frozen.md) and
stops if a byte is unaccounted for. It also checks two things a receiver
checks: the carrier's bytes hash to the id the envelope names, and the
funding transaction's bytes hash to the id the carrier spends.
