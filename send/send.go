// Package send is beefcat's near end: it cuts a stream of bytes into
// carriers, each spending one mined funding output of the home's, and
// writes them as frames.
//
// Sending asks nothing of the chain. The funding trees were mined ahead
// (beefcat fund), each kept with its Merkle path, and every carrier carries
// the tree it spends from and that path, so the far end verifies with
// nothing but its own block headers.
//
// A funding output is marked spent in the home's state before its carrier
// is signed: a run that stops part way never signs a second carrier on the
// same output.
package send

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"

	"github.com/lightwebinc/beefcat/internal/home"
	"github.com/lightwebinc/beefcat/wire"
)

// ErrNoPostage is a home with no mined funding output left.
var ErrNoPostage = errors.New("out of postage: no mined funding output is left (run `beefcat fund`)")

// Sender sends streams from one home.
type Sender struct {
	Wallet     wallet.Interface
	Originator string
	Identity   [33]byte
	State      *home.State
	// Chunk is the most data one carrier holds; zero is wire.MaxData.
	Chunk int
	// Lines sends each line of input as it arrives, one carrier a line,
	// and closes the stream with an empty last carrier.
	Lines bool

	trees map[string]*transaction.Transaction
}

// Stats are what one stream sent.
type Stats struct {
	Stream   [16]byte
	Carriers int
	Bytes    int64
	// Wire is the bytes written: frames and preamble.
	Wire int64
}

// Need is how many carriers a stream of size bytes takes in chunks of
// chunk (zero is wire.MaxData).
func Need(size int64, chunk int) uint64 {
	if chunk <= 0 {
		chunk = wire.MaxData
	}
	if size <= 0 {
		return 1
	}
	return uint64((size + int64(chunk) - 1) / int64(chunk)) //nolint:gosec // positive
}

// Stream reads r to its end and writes it to w as one stream of carriers.
func (s *Sender) Stream(ctx context.Context, r io.Reader, w io.Writer) (Stats, error) {
	var st Stats
	chunk := s.Chunk
	if chunk <= 0 || chunk > wire.MaxData {
		chunk = wire.MaxData
	}
	if _, err := rand.Read(st.Stream[:]); err != nil {
		return st, err
	}
	if s.State.NextTree() == nil {
		return st, ErrNoPostage
	}
	if err := wire.WritePreamble(w); err != nil {
		return st, err
	}
	st.Wire = int64(len(wire.Preamble))
	var prev [32]byte
	emit := func(data []byte, last bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		tree, vout, err := s.take(len(data))
		if err != nil {
			return err
		}
		h := wire.Header{Last: last, Stream: st.Stream, Seq: uint64(st.Carriers), Prev: prev, Identity: s.Identity} //nolint:gosec // a count
		payload := append(h.Append(make([]byte, 0, wire.HeaderLen+len(data))), data...)
		tx, err := carrier.Mint(ctx, s.Wallet, s.Originator, wire.Params, payload, tree, vout)
		if err != nil {
			return fmt.Errorf("carrier %d: %w", st.Carriers, err)
		}
		beef, err := funding.BEEF(tx)
		if err != nil {
			return fmt.Errorf("carrier %d: %w", st.Carriers, err)
		}
		if err := wire.WriteFrame(w, beef); err != nil {
			return fmt.Errorf("carrier %d: %w", st.Carriers, err)
		}
		prev = carrier.Commitment(tx)
		st.Carriers++
		st.Bytes += int64(len(data))
		st.Wire += int64(4 + len(beef))
		return nil
	}
	if s.Lines {
		br := bufio.NewReaderSize(r, chunk)
		for {
			line, err := br.ReadSlice('\n')
			// A line longer than a chunk goes out in chunks.
			for len(line) > 0 {
				n := min(len(line), chunk)
				if eerr := emit(line[:n], false); eerr != nil {
					return st, eerr
				}
				line = line[n:]
			}
			if err == io.EOF {
				return st, emit(nil, true)
			}
			if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
				return st, err
			}
		}
	}
	// One chunk is read ahead, so the last carrier is known as it is
	// signed and an input that ends on a chunk boundary costs no extra one.
	cur, next := make([]byte, chunk), make([]byte, chunk)
	n, err := readChunk(r, cur)
	if err != nil {
		return st, err
	}
	for {
		m, err := readChunk(r, next)
		if err != nil {
			return st, err
		}
		if m == 0 {
			return st, emit(cur[:n], true)
		}
		if err := emit(cur[:n], false); err != nil {
			return st, err
		}
		cur, next, n = next, cur, m
	}
}

// readChunk fills b from r and reports how much it read; zero is the end.
func readChunk(r io.Reader, b []byte) (int, error) {
	n, err := io.ReadFull(r, b)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		err = nil
	}
	return n, err
}

// take marks the next funding output spent, saves the state, and returns
// the tree and the output.
func (s *Sender) take(size int) (*transaction.Transaction, uint32, error) {
	t := s.State.NextTree()
	if t == nil {
		return nil, 0, ErrNoPostage
	}
	tx, ok := s.trees[t.Txid]
	if !ok {
		var err error
		if tx, err = funding.Rebuild(t.RawHex, t.BumpHex, ""); err != nil {
			return nil, 0, fmt.Errorf("funding tree %s: %w", t.Txid, err)
		}
		if tx.TxID().String() != t.Txid || tx.MerklePath == nil {
			return nil, 0, fmt.Errorf("funding tree %s: the home's record does not rebuild it with its proof", t.Txid)
		}
		if s.trees == nil {
			s.trees = map[string]*transaction.Transaction{}
		}
		s.trees[t.Txid] = tx
	}
	vout := t.Next
	t.Next++
	s.State.SentCarriers++
	s.State.SentBytes += uint64(size) //nolint:gosec // a length
	if err := s.State.Save(); err != nil {
		return nil, 0, err
	}
	return tx, vout, nil
}
