# Quickstart

Two ways in. **In the sandbox** everything runs on one machine over a
local stand-in chain, and no coin is needed. **On mainnet** the sender
pays for postage once from its own wallet; the receiver needs nothing but
the binary.

## In the sandbox

Build the two commands from a checkout:

```console
$ make beefcat devchain
$ export PATH=$PWD/bin:$PATH
```

Start the local chain in one terminal. It is a stand-in for a node, a
broadcaster and a header source; it mines every transaction at once, and
nothing it mines is money.

```console
$ devchain -listen 127.0.0.1:18080
```

Create the sender's home and mine postage for 8 carriers:

```console
$ beefcat init -devchain http://127.0.0.1:18080
created /home/user/.beefcat
identity     02c6...9a1e
fund address n1AE...Y8X (regtest)
$ beefcat fund -n 8 -coinbase
postage for 8 carrier(s), up to 33554432 bytes of data; pool 101 output(s), ...
```

Listen in a second terminal, pinning the sender's identity key:

```console
$ beefcat -l 127.0.0.1:9000 -network regtest -headers http://127.0.0.1:18080 \
    -k 02c6...9a1e > received.bin
```

Send:

```console
$ head -c 5000000 /dev/urandom > sent.bin
$ beefcat 127.0.0.1 9000 < sent.bin
beefcat: sent 2 carrier(s), 5000000 bytes of data ...
beefcat: the far end says: VERIFIED carriers=2 bytes=5000000
$ cmp sent.bin received.bin
```

### Now break it

Write a stream to a file, so it can be tampered with:

```console
$ beefcat -o stream.bcat < sent.bin
$ RX="-network regtest -headers http://127.0.0.1:18080"
```

| Try | Command | Outcome |
| --- | --- | --- |
| Cut it short | `head -c 3000000 stream.bcat \| beefcat verify $RX -k 02c6...9a1e` | `REFUSED-TRUNCATED`, exit 3 |
| Lose a byte | `{ head -c 2000000 stream.bcat; tail -c +2000002 stream.bcat; } \| beefcat verify $RX -k 02c6...9a1e` | refused, exit 3 |
| Expect someone else | `beefcat verify $RX -k 0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798 stream.bcat` | `REFUSED-UNTRUSTED`, exit 4 |
| Expect no one | `beefcat verify $RX stream.bcat` | verified, with a loud warning |
| Retract it | `beefcat sweep -yes`, then `beefcat verify $RX -chain asset:http://127.0.0.1:18080 -unspent -k 02c6...9a1e stream.bcat` | `REFUSED-RETRACTED`, exit 3 |

`make quickstart-check` runs this page as a script.

## On mainnet

The sender needs a wallet holding a little BSV. No node is needed: the
defaults read headers and the chain from WhatsOnChain and broadcast
through a public arcade. Your own header service or node is better; see
[docs/usage.md](docs/usage.md#sources).

Create the home and note the fund address and the identity key:

```console
$ beefcat init
created /home/user/.beefcat
identity     02c6...9a1e
fund address 1Kq3...Vb7 (main)
```

Pay the fund address from your own wallet. Postage for 64 carriers
(256 MiB of data) costs about 460 satoshis; a few thousand satoshis is
plenty. Once that payment is mined, import it and mint the postage:

```console
$ beefcat fund -n 64 -txid 5e1f...77ab
```

`fund` broadcasts one funding transaction and waits for the block that
mines it, about ten minutes on average. If it times out, run the same
command again: it asks what became of the transaction and never mints
twice.

Give the receiver your identity key. Then, on the receiver:

```console
$ beefcat -l 9000 -k 02c6...9a1e > received.bin
```

and on the sender:

```console
$ beefcat receiver.example.com 9000 < file
```

Sending asks nothing of the chain: the postage is already mined, and
every carrier carries its proof.
