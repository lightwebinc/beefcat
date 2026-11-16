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
	"runtime"
	"sync"

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
	// Workers is how many carriers are signed at once; zero is one per
	// core, at most 16.
	Workers int

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
//
// Input is cut into chunks in order, each given its sequence number and a
// funding output as it is read. No carrier depends on another's bytes, so
// they are signed on several cores, and written in the order they were
// read.
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
	workers := s.Workers
	if workers <= 0 {
		workers = min(runtime.GOMAXPROCS(0), 16)
	}
	if s.Lines {
		// A line goes out as it is read: nothing is read ahead of it.
		workers = 1
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	// One goroutine reads, numbers and funds the chunks, in order, and
	// queues each carrier's frame as a promise; this one writes them in
	// that order.
	type item struct {
		frame chan signed
		err   error
	}
	queue := make(chan item, workers)
	bufs := sync.Pool{New: func() any { b := make([]byte, 0, wire.HeaderLen+chunk); return &b }}
	go func() {
		defer close(queue)
		slots := make(chan struct{}, workers)
		seq := uint64(0)
		fail := func(err error) {
			select {
			case queue <- item{err: err}:
			case <-ctx.Done():
			}
		}
		// emit hands one chunk to a worker. data is the reader's buffer,
		// so it is copied behind its header before emit returns.
		emit := func(data []byte, last bool) bool {
			tree, vout, err := s.take(len(data))
			if err != nil {
				fail(err)
				return false
			}
			h := wire.Header{Last: last, Stream: st.Stream, Seq: seq, Identity: s.Identity}
			seq++
			bp := bufs.Get().(*[]byte)
			payload := append(h.Append((*bp)[:0]), data...)
			it := item{frame: make(chan signed, 1)}
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return false
			}
			go func() {
				defer func() { <-slots }()
				tx, err := carrier.Mint(ctx, s.Wallet, s.Originator, wire.Params, payload, tree, vout)
				var beef []byte
				if err == nil {
					beef, err = funding.BEEF(tx)
				}
				*bp = payload[:0]
				bufs.Put(bp)
				it.frame <- signed{beef: beef, data: len(data), err: err}
			}()
			select {
			case queue <- it:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if s.Lines {
			br := bufio.NewReaderSize(r, max(chunk, 16))
			for {
				line, err := br.ReadSlice('\n')
				// A line longer than a chunk goes out in chunks.
				for len(line) > 0 {
					n := min(len(line), chunk)
					if !emit(line[:n], false) {
						return
					}
					line = line[n:]
				}
				if err == io.EOF {
					emit(nil, true)
					return
				}
				if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
					fail(err)
					return
				}
			}
		}
		// One chunk is read ahead, so the last carrier is known as it is
		// signed and an input that ends on a chunk boundary costs no extra
		// one.
		cur, next := make([]byte, chunk), make([]byte, chunk)
		n, err := readChunk(r, cur)
		if err != nil {
			fail(err)
			return
		}
		for {
			m, err := readChunk(r, next)
			if err != nil {
				fail(err)
				return
			}
			if m == 0 {
				emit(cur[:n], true)
				return
			}
			if !emit(cur[:n], false) {
				return
			}
			cur, next, n = next, cur, m
		}
	}()

	for it := range queue {
		if it.err != nil {
			return st, it.err
		}
		f := <-it.frame
		if f.err != nil {
			return st, fmt.Errorf("carrier %d: %w", st.Carriers, f.err)
		}
		if err := wire.WriteFrame(w, f.beef); err != nil {
			return st, fmt.Errorf("carrier %d: %w", st.Carriers, err)
		}
		st.Carriers++
		st.Bytes += int64(f.data)
		st.Wire += int64(4 + len(f.beef))
	}
	return st, ctx.Err()
}

// signed is one carrier's frame, or why it could not be made.
type signed struct {
	beef []byte
	data int
	err  error
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
// the tree and the output. Only the reading goroutine calls it.
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
