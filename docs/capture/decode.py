#!/usr/bin/env python3
"""Decode a beefcat capture into packets and named byte fields (JSON)."""
import json, struct, sys, hashlib

src = sys.argv[1]
d = open(src, 'rb').read()
magic, _, _, _, _, _, link = struct.unpack('<IHHiIII', d[:24])
assert magic == 0xa1b2c3d4 and link == 1
off, pk = 24, []
while off < len(d):
    ts, us, cap, _ = struct.unpack('<IIII', d[off:off + 16]); off += 16
    pk.append((ts + us / 1e6, d[off:off + cap])); off += cap

t0 = pk[0][0]
packets, streams, port = [], {True: b'', False: b''}, None
isn = {}
for t, p in pk:
    ihl = (p[14] & 15) * 4
    tcp = 14 + ihl
    sp, dp, seq, ack = struct.unpack('>HHII', p[tcp:tcp + 12])
    doff = (p[tcp + 12] >> 4) * 4
    fl = p[tcp + 13]
    pay = p[tcp + doff:]
    if port is None:
        port = dp
    to_rx = dp == port
    isn.setdefault(to_rx, seq)
    flags = ''.join(n for b, n in ((2, 'SYN '), (1, 'FIN '), (8, 'PSH '), (16, 'ACK ')) if fl & b).strip()
    packets.append(dict(t=round((t - t0) * 1000, 3), toRx=to_rx, flags=flags, len=len(pay),
                        seq=(seq - isn[to_rx]) & 0xffffffff, sp=sp, dp=dp))
    streams[to_rx] += pay

c = streams[True]
fields = []
pos = 0

def f(n, cat, name, note='', val=None, sec=None):
    global pos
    b = c[pos:pos + n]
    assert len(b) == n, (name, pos, n)
    fields.append(dict(s=pos, e=pos + n, cat=cat, name=name, note=note, val=val if val is not None else '', sec=sec or cur))
    pos += n
    return b

def varint(cat, name, note=''):
    p = c[pos]
    n = {0xfd: 3, 0xfe: 5, 0xff: 9}.get(p, 1)
    v = p if n == 1 else int.from_bytes(c[pos + 1:pos + n], 'little')
    f(n, cat, name, note, str(v))
    return v

def rev(b):
    return b[::-1].hex()

cur = 'Stream'
f(8, 'wire', 'Preamble', 'Opens every stream: the letters BEEFCAT and the framing version.', 'BEEFCAT + 0x01')
flen = int.from_bytes(f(4, 'wire', 'Frame length', 'Big-endian byte count of the one frame that follows. One frame is one carrier.', None), 'big')
fields[-1]['val'] = f'{flen:,} bytes'
frame_end = pos + flen

cur = 'Atomic BEEF envelope'
f(4, 'proof', 'Atomic BEEF marker', 'BRC-95: this envelope proves exactly one subject transaction.', '0x01010101')
subject = f(32, 'proof', 'Subject txid', 'The carrier this envelope is about. A receiver refuses the frame if these are not the bytes of that transaction.', None)
fields[-1]['val'] = rev(subject)
f(4, 'proof', 'BEEF version', 'BRC-96, BEEF V2.', '0xEFBE0002')

cur = 'Merkle path (BUMP)'
varint('proof', 'Path count', 'One Merkle path: the funding transaction\'s.')
height = varint('proof', 'Block height', 'The block the funding transaction was mined in. The receiver asks its own header source for this block\'s Merkle root.')
th = f(1, 'proof', 'Tree height', 'Levels from the transaction up to the root.')[0]
fields[-1]['val'] = str(th)
for lvl in range(th):
    n = varint('proof', f'Level {lvl}: leaf count')
    for _ in range(n):
        o = varint('proof', f'Level {lvl}: offset', 'Position of this hash in its level of the block\'s Merkle tree.')
        fl = f(1, 'proof', f'Level {lvl}: flags', '0 = a sibling hash follows, 1 = duplicate the working hash, 2 = this is the transaction\'s own id.')[0]
        fields[-1]['val'] = str(fl)
        if fl & 1 == 0:
            h = f(32, 'proof', f'Level {lvl}: hash', 'The funding transaction\'s id.' if fl == 2 else 'A sibling hash. Hashed with the working hash to climb one level.')
            fields[-1]['val'] = rev(h)

cur = 'Transactions'
ntx = varint('proof', 'Transaction count', 'Two: the mined funding transaction, then the carrier that spends it.')

def tx(kind):
    global cur, pos
    start = pos
    chain = kind == 'funding'
    cat = 'chain' if chain else 'carrier'
    f(4, cat, 'Version', '', '1')
    nin = varint(cat, 'Input count')
    for i in range(nin):
        ptx = f(32, 'proof' if not chain else cat, 'Input: previous txid',
                'The funding transaction. This is the link SPV follows: the carrier is never mined, but it spends an output of a transaction that was.' if not chain else 'The coin that paid for the funding tree.')
        fields[-1]['val'] = rev(ptx)
        v = int.from_bytes(f(4, 'proof' if not chain else cat, 'Input: output index',
                             'Which funding output this carrier spends. One output pays for one carrier.' if not chain else ''), 'little')
        fields[-1]['val'] = str(v)
        sl = varint('party', 'Unlocking script length')
        if not chain:
            push = f(1, 'party', 'Push opcode', 'One push, and nothing else: a receiver refuses any other unlocking script, so nobody can rewrite the carrier under another id.')[0]
            fields[-1]['val'] = f'push {push} bytes'
            f(push - 1, 'party', 'Input signature (DER)', 'The sender\'s ECDSA signature over the whole carrier, including its data. It satisfies the funding output\'s lock.')
            fields[-1]['val'] = f'{push - 1} bytes'
            f(1, 'party', 'Sighash type', 'SIGHASH_ALL | FORKID: the signature covers every input and output.', '0x41')
        else:
            f(sl, 'party', 'Unlocking script', 'Signature and public key that spent the payment into this tree.', f'{sl} bytes')
        s = int.from_bytes(f(4, cat, 'Input: sequence', 'Zero, not final. With the lock time below, this keeps the carrier out of every block.' if not chain else 'Final.'), 'little')
        fields[-1]['val'] = f'0x{s:08x}'
    nout = varint(cat, 'Output count', 'One output: the record.' if not chain else '64 funding outputs and one change output.')
    if chain:
        first = pos
        spent_at = None
        for i in range(nout):
            o0 = pos
            val = int.from_bytes(c[pos:pos + 8], 'little')
            p = pos + 8
            sl = c[p]; assert sl < 0xfd
            total = 8 + 1 + sl
            if i == SPENT:
                # close the run before, describe this one field by field
                if pos > first:
                    fields.append(dict(s=first, e=pos, cat='chain', name=f'Funding outputs 0 to {i - 1}', sec=cur, fold=True,
                                       note='Each is one satoshi locked to the sender\'s key with the tag "beefcat fund". Each pays for one carrier.', val=f'{i} outputs, {pos - first:,} bytes'))
                f(8, 'chain', f'Funding output {i}: value', 'One satoshi. This is the output the carrier spends.', '1 satoshi')
                varint('chain', f'Funding output {i}: script length')
                f(1, 'party', 'Push opcode', '', 'push 33 bytes')
                f(33, 'party', 'Sender\'s derived key', 'The key the sender\'s identity key derives. The carrier is locked by the same key, which is how a receiver ties both to one sender.')
                fields[-1]['val'] = fields[-1].get('val') or c[pos - 33:pos].hex()
                f(1, 'chain', 'OP_CHECKSIG', 'Spendable by one signature from that key.', '0xac')
                tl = f(1, 'chain', 'Push opcode', '', None)[0]
                fields[-1]['val'] = f'push {tl} bytes'
                tag = f(tl, 'chain', 'Funding tag', 'Marks the output as postage.', None)
                fields[-1]['val'] = '"' + tag.decode() + '"'
                f(1, 'chain', 'OP_DROP', '', '0x75')
                first = pos
            else:
                pos += total
        if pos > first:
            fields.append(dict(s=first, e=pos, cat='chain', name=f'Funding outputs {SPENT + 1} to {nout - 2}, and change', sec=cur, fold=True,
                               note='The rest of the postage, unspent, and the change returned to the sender\'s pool.', val=f'{nout - 1 - SPENT} outputs, {pos - first:,} bytes'))
    else:
        v = int.from_bytes(f(8, cat, 'Output value', 'The funding output\'s satoshi, carried forward. The carrier pays no fee, because it is never mined.'), 'little')
        fields[-1]['val'] = f'{v} satoshi'
        sl = varint(cat, 'Locking script length')
        f(1, 'party', 'Push opcode', '', 'push 33 bytes')
        k = f(33, 'party', 'Sender\'s derived key', 'Derived from the identity key named in the payload. A receiver recomputes it from that identity and refuses a mismatch.')
        fields[-1]['val'] = k.hex()
        f(1, cat, 'OP_CHECKSIG', '', '0xac')
        op = c[pos]
        if op == 0x4c:
            f(1, cat, 'OP_PUSHDATA1', ''); plen = f(1, cat, 'Payload length', '')[0]
        elif op == 0x4d:
            f(1, cat, 'OP_PUSHDATA2', ''); plen = int.from_bytes(f(2, cat, 'Payload length', ''), 'little')
        elif op == 0x4e:
            f(1, cat, 'OP_PUSHDATA4', ''); plen = int.from_bytes(f(4, cat, 'Payload length', ''), 'little')
        else:
            plen = f(1, cat, 'Push opcode', '')[0]
        fields[-1]['val'] = f'{plen} bytes'
        pend = pos + plen
        cur = 'Signed payload'
        f(4, 'carrier', 'Magic', 'Marks a beefcat record.', '"BCAT"')
        f(1, 'carrier', 'Version', '', '1')
        fl = f(1, 'carrier', 'Flags', 'Bit 0 set: this is the last carrier of the stream. The end is signed, so a stream cut short is refused.')[0]
        fields[-1]['val'] = f'0x{fl:02x}' + (' (last)' if fl & 1 else '')
        sid = f(16, 'carrier', 'Stream id', 'Random per stream. Carriers of another stream cannot be spliced in.')
        fields[-1]['val'] = sid.hex()
        sq = int.from_bytes(f(8, 'carrier', 'Sequence', 'Position in the stream, from 0. Order is signed.'), 'big')
        fields[-1]['val'] = str(sq)
        idk = f(33, 'party', 'Sender\'s identity key', 'Who claims to have sent this. The receiver pins this key with -k.')
        fields[-1]['val'] = idk.hex()
        data = f(pend - pos, 'data', 'Data', 'The sender\'s bytes, exactly as given. Written out only after every check has passed.')
        fields[-1]['val'] = data.decode('utf-8', 'replace')
        cur = 'Carrier (never mined)'
        sp = f(1, 'party', 'Push opcode', '')[0]
        fields[-1]['val'] = f'push {sp} bytes'
        f(sp, 'party', 'Field signature (DER)', 'The sender\'s signature over SHA-256 of the payload: the data is the key holder\'s, whatever happens to the spend.', f'{sp} bytes')
        f(1, cat, 'OP_2DROP', '', '0x6d')
    lt = int.from_bytes(f(4, cat, 'Lock time', '1 January 2100. No node may mine this before then: a consensus rule, not a policy.' if not chain else 'None.'), 'little')
    fields[-1]['val'] = f'{lt:,}' + (' (2100-01-01 UTC)' if lt == 4102444800 else '')
    raw = c[start:pos]
    return hashlib.sha256(hashlib.sha256(raw).digest()).digest()

# which funding output the carrier spends: read ahead in the carrier
def peek_spent():
    # the carrier is the last transaction: find it by walking the first one
    p = pos
    fmt = c[p]; p += 1
    if fmt == 1:
        p += 1
    p += 4
    nin = c[p]; p += 1
    for _ in range(nin):
        p += 36
        sl = c[p]; p += 1 + sl + 4
    nout = c[p]; p += 1
    for _ in range(nout):
        p += 8
        sl = c[p]; p += 1 + sl
    p += 4
    p += 1 + 4 + 1 + 32  # format, version, input count, prev txid
    return int.from_bytes(c[p:p + 4], 'little')

SPENT = peek_spent()
ids = {}
for i in range(ntx):
    kind = 'funding' if i == 0 else 'carrier'
    cur = 'Funding transaction (mined)' if i == 0 else 'Carrier (never mined)'
    fmt = f(1, 'proof', 'Entry format', '1 = a raw transaction with a Merkle path.' if i == 0 else '0 = a raw transaction with no proof of its own. A carrier never has one.')[0]
    fields[-1]['val'] = str(fmt)
    if fmt == 1:
        varint('proof', 'Path index', 'Which Merkle path above proves this transaction.')
    ids[kind] = tx(kind)
assert pos == frame_end == len(c), (pos, frame_end, len(c))
assert ids['carrier'] == subject

reply = streams[False].decode()
out = dict(
    packets=packets, port=port, hex=c.hex(), fields=fields, reply=reply.strip(),
    facts=dict(bytes=len(c), frame=flen, height=height, spent=SPENT,
               funding=rev(ids['funding']), carrier=rev(ids['carrier']),
               data=next(x['e'] - x['s'] for x in fields if x['name'] == 'Data'),
               verdictMs=next(p['t'] for p in packets if not p['toRx'] and p['len'] > 0),
               sentMs=next(p['t'] for p in packets if p['toRx'] and p['len'] > 0)))
json.dump(out, open(sys.argv[2], 'w'))
print(len(fields), 'fields;', len(packets), 'packets; funding', out['facts']['funding'], 'carrier', out['facts']['carrier'], 'spends output', SPENT)
for x in fields:
    if x.get('fold') or x['name'] in ('Data', 'Lock time', 'Block height', 'Frame length'):
        print(' ', x['s'], x['e'], x['name'], '|', str(x['val'])[:60])
