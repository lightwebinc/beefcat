// Package recv is beefcat's far end: it reads a stream of carriers and
// writes a carrier's data only after that carrier has passed every check.
//
// The checks, in order, for each carrier:
//
//  1. the frame is within bounds, is Atomic BEEF holding no more than a
//     carrier and what it spends, and parses (package guard);
//  2. the transaction comes with no proof of its own, and is a carrier
//     with one beefcat record;
//  3. the payload follows its rules, the carrier cannot be mined, its input
//     is one canonical signature, its lock is the key the named identity
//     derives, and the field signature verifies over the payload;
//  4. the carrier continues the stream: same sender, same stream, the next
//     sequence;
//  5. it spends a beefcat funding output of a transaction that carries its
//     own Merkle path, a path that leads from that transaction, and an
//     output no earlier carrier of the stream spent;
//  6. SPV: the input satisfies that output, and the path leads to a root
//     the reader's own header source holds;
//  7. when asked, the funding output is not spent by another transaction,
//     which is how a sender retracts;
//  8. the sender is one the reader trusts, when the reader pinned any.
//
// Everything but 6 and 7 is arithmetic on the bytes received. A carrier
// that fails is refused with a code from bcommon's verify vocabulary and
// nothing of it is written; a stream that ends before its signed last
// carrier is refused as truncated.
package recv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/verify"

	"github.com/lightwebinc/beefcat/wire"
)

// Codes beefcat adds to bcommon's vocabulary.
const (
	// RefusedTruncated is a stream that ended before its last carrier.
	RefusedTruncated verify.Code = "REFUSED-TRUNCATED"
	// RefusedRetracted is a carrier whose funding output another
	// transaction spent: the sender retracted it, or it was never the
	// output's one spender.
	RefusedRetracted verify.Code = "REFUSED-RETRACTED"
	// RefusedUntrusted is a sender the reader did not pin.
	RefusedUntrusted verify.Code = "REFUSED-UNTRUSTED"
)

// Options are a reader's choices.
type Options struct {
	// Headers is the reader's own header source. Required.
	Headers chaintracker.ChainTracker
	// Trusted are the identity keys the reader takes data from. Empty
	// takes any sender whose carriers verify, and Summary.Pinned is false.
	Trusted [][33]byte
	// Spends, when set, is asked whether each funding output is spent by
	// another transaction.
	Spends nodeapi.SpendSource
	// Workers is how many carriers are checked at once; zero is one per
	// core, at most 16. Each holds one carrier, so it also bounds memory.
	Workers int
}

// Summary is what was verified and written.
type Summary struct {
	// Identity is the sender's identity key, once a carrier named one that
	// its signatures bear out.
	Identity     [33]byte
	HaveIdentity bool
	// Pinned reports that Identity is one of Options.Trusted.
	Pinned bool
	Stream [16]byte
	// Carriers and Bytes count what was verified and written.
	Carriers int
	Bytes    int64
	// Tree and Height are the funding transaction and block of the last
	// carrier verified.
	Tree   string
	Height uint32
	// Unspent reports that every funding output was checked unspent.
	Unspent bool
}

// Refusal is a verdict: the stream did not verify. Carrier is the index of
// the carrier refused, from 0.
type Refusal struct {
	Code    verify.Code
	Reason  string
	Carrier int
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("%s: carrier %d: %s", r.Code, r.Carrier, r.Reason)
}

// Receive reads a stream from r and writes its data to w. It returns what
// it verified and wrote, and nil only when the whole stream verified
// through its signed last carrier. A *Refusal is a verdict; any other
// error means the reader could not decide (the header source did not
// answer, or reading or writing failed), which is never a verdict.
//
// A carrier's own checks need nothing of the carriers around it, so they
// run on several cores, a few carriers ahead of the one being written. What
// ties carriers together (the sender, the stream, the sequence, the funding
// outputs already spent) is checked in order, by the one goroutine that
// writes, so the verdict is the one a reader working a carrier at a time
// would reach: the first carrier that fails, for the first check it fails.
func Receive(ctx context.Context, r io.Reader, w io.Writer, o Options) (Summary, error) {
	var s Summary
	if o.Headers == nil {
		return s, verify.ErrNoTracker
	}
	refuse := func(code verify.Code, format string, args ...any) (Summary, error) {
		return s, &Refusal{Code: code, Reason: fmt.Sprintf(format, args...), Carrier: s.Carriers}
	}
	failed := func(f *fail) (Summary, error) {
		if f.err != nil {
			return s, fmt.Errorf("carrier %d: %w", s.Carriers, f.err)
		}
		return refuse(f.code, "%s", f.reason)
	}
	if err := wire.ReadPreamble(r); err != nil {
		return refuse(verify.RefusedDecode, "%v", err)
	}
	workers := o.Workers
	if workers <= 0 {
		workers = min(runtime.GOMAXPROCS(0), 16)
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	headers := &memoTracker{t: o.Headers, roots: map[rootKey]*rootAnswer{}}

	// The frames are read in order by one goroutine, which hands each to a
	// worker and queues its answer, so the answers come out in the order
	// the frames went in. The queue's length bounds what is held ahead.
	type item struct {
		checked chan *checked // one carrier's answer
		readErr error         // or how reading ended
	}
	queue := make(chan item, workers)
	go func() {
		defer close(queue)
		slots := make(chan struct{}, workers)
		for {
			frame, err := wire.ReadFrame(r)
			if err != nil {
				select {
				case queue <- item{readErr: err}:
				case <-ctx.Done():
				}
				return
			}
			it := item{checked: make(chan *checked, 1)}
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				defer func() { <-slots }()
				it.checked <- check(ctx, frame, o, headers)
			}()
			select {
			case queue <- it:
			case <-ctx.Done():
				return
			}
		}
	}()

	spent := map[string]bool{}
	last := false
	for it := range queue {
		if last {
			// Anything after the signed last carrier is not part of the
			// stream: only its clean end may follow.
			if it.readErr == io.EOF {
				return s, nil
			}
			if it.readErr != nil && !errors.Is(it.readErr, io.ErrUnexpectedEOF) && !errors.Is(it.readErr, wire.ErrFrame) {
				return s, fmt.Errorf("reading the end of the stream: %w", it.readErr)
			}
			return refuse(verify.RefusedSeq, "bytes follow the signed last carrier: something was appended to the stream")
		}
		switch err := it.readErr; {
		case err == io.EOF:
			return refuse(RefusedTruncated, "the stream ended after %d carrier(s) with no signed last carrier: it was cut short", s.Carriers)
		case errors.Is(err, io.ErrUnexpectedEOF):
			return refuse(RefusedTruncated, "the stream ended inside a frame: it was cut short")
		case errors.Is(err, wire.ErrFrame):
			return refuse(verify.RefusedDecode, "%v", err)
		case err != nil:
			return s, fmt.Errorf("reading carrier %d: %w", s.Carriers, err)
		}
		c := <-it.checked
		if c.own != nil {
			return failed(c.own)
		}
		h := c.header
		first := s.Carriers == 0
		if first {
			s.Stream = h.Stream
		}
		switch {
		case first:
		case h.Identity != s.Identity:
			return refuse(verify.RefusedKey, "signed by %x, but the stream began under %x", h.Identity, s.Identity)
		case h.Stream != s.Stream:
			return refuse(verify.RefusedSeq, "belongs to stream %x, not %x: carriers of another stream were spliced in", h.Stream, s.Stream)
		case h.Seq != uint64(s.Carriers): //nolint:gosec // a count
			return refuse(verify.RefusedSeq, "sequence %d where %d was due: carriers are missing, repeated or out of order", h.Seq, s.Carriers)
		}
		if c.funding != nil {
			return failed(c.funding)
		}
		if spent[c.outpoint] {
			return refuse(verify.RefusedFork, "funding output %s is spent by an earlier carrier of this stream", c.outpoint)
		}
		if c.proof != nil {
			return failed(c.proof)
		}
		if c.retracted != nil {
			return failed(c.retracted)
		}
		// Trust is asked last, of a carrier that verified in full, so
		// "untrusted" always means: genuine, and not from a sender you
		// pinned.
		if len(o.Trusted) > 0 && !trusted(o.Trusted, h.Identity) {
			return refuse(RefusedUntrusted, "this data verified, but its sender %x is not a key you pinned", h.Identity)
		}
		if first {
			s.Identity, s.HaveIdentity, s.Pinned = h.Identity, true, len(o.Trusted) > 0
		}
		if _, err := w.Write(c.data); err != nil {
			return s, fmt.Errorf("writing carrier %d: %w", s.Carriers, err)
		}
		spent[c.outpoint] = true
		s.Carriers++
		s.Bytes += int64(len(c.data))
		s.Tree, s.Height = c.tree, c.height
		s.Unspent = o.Spends != nil
		last = h.Last
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	return s, errors.New("the reader stopped without an answer")
}

// fail is one check that did not pass: a refusal, or with err set, a check
// that could not be made.
type fail struct {
	code   verify.Code
	reason string
	err    error
}

func refusal(code verify.Code, format string, args ...any) *fail {
	return &fail{code: code, reason: fmt.Sprintf(format, args...)}
}

// checked is what one carrier's own checks found, kept apart by where each
// stands in the order of the checks, so the goroutine that works in order
// can put the checks between carriers where they belong.
type checked struct {
	// own is a failure of the carrier as it stands: frame, shape, payload,
	// finality, unlocking script, lock, signature.
	own    *fail
	header wire.Header
	data   []byte
	// funding is a failure of what the carrier spends, before SPV.
	funding  *fail
	outpoint string
	tree     string
	height   uint32
	// proof is SPV's failure, retracted the spend check's.
	proof, retracted *fail
}

// check runs every check one carrier needs nothing else for.
func check(ctx context.Context, frame []byte, o Options, headers chaintracker.ChainTracker) *checked {
	c := &checked{}
	// A frame is Atomic BEEF and nothing else, holding a carrier and the
	// transaction it spends. Both are checked before the SDK parses a
	// byte: a BEEF of many chained transactions costs its reader time
	// that doubles with each one.
	if !wire.IsAtomic(frame) {
		c.own = refusal(verify.RefusedDecode, "the frame is not Atomic BEEF")
		return c
	}
	_, tx, txid, err := guard.ParseBEEFWithin(frame, wire.MaxFrame, frameLimits)
	if err != nil || tx == nil || txid == nil {
		c.own = refusal(verify.RefusedDecode, "BEEF does not parse: %v", err)
		return c
	}
	if tx.MerklePath != nil {
		c.own = refusal(verify.RefusedBump, "the carrier comes with a Merkle path of its own: a carrier is never mined, so the proof is forged")
		return c
	}
	car, err := carrier.Decode(tx, wire.Classify)
	if err != nil {
		c.own = refusal(verify.RefusedDecode, "not a carrier: %v", err)
		return c
	}
	h, data, err := wire.Parse(car.Payload)
	if err != nil {
		c.own = refusal(verify.RefusedDecode, "%v", err)
		return c
	}
	c.header, c.data = h, data
	if err := car.Validate(wire.Params, h.Identity[:]); err != nil {
		switch {
		case errors.Is(err, carrier.ErrMineable):
			c.own = refusal(verify.RefusedMineable, "%v", err)
		case errors.Is(err, carrier.ErrUnlocking):
			c.own = refusal(verify.RefusedUnlocking, "%v", err)
		case errors.Is(err, carrier.ErrLock):
			c.own = refusal(verify.RefusedKeyDerive, "%v: the carrier is not locked by the identity it names", err)
		case errors.Is(err, carrier.ErrSignature):
			c.own = refusal(verify.RefusedSig, "%v: the data is not what the sender signed", err)
		default:
			c.own = refusal(verify.RefusedDecode, "%v", err)
		}
		return c
	}
	in := tx.Inputs[0]
	src := in.SourceTransaction
	if src == nil || uint64(in.SourceTxOutIndex) >= uint64(len(src.Outputs)) {
		c.funding = refusal(verify.RefusedBump, "the funding transaction is not in the BEEF")
		return c
	}
	tree := src.TxID()
	if src.MerklePath == nil {
		c.funding = refusal(verify.RefusedBump, "the funding transaction %s carries no Merkle path: its postage was never mined", tree)
		return c
	}
	key, ok := carrier.DecodeFunding(src.Outputs[in.SourceTxOutIndex].LockingScript, wire.FundingTag)
	if !ok || !key.IsEqual(car.LockingKey) {
		c.funding = refusal(verify.RefusedDecode, "the input does not spend a beefcat funding output of the sender's")
		return c
	}
	// A path that does not lead from the funding transaction to a root
	// is a forged proof, and is refused here by arithmetic. Left to
	// SPV it would surface as an error with no verdict, as if the
	// header source had failed.
	if _, err := src.MerklePath.ComputeRoot(tree); err != nil {
		c.funding = refusal(verify.RefusedBump, "the Merkle path given for the funding transaction %s does not lead from it: %v", tree, err)
		return c
	}
	c.tree, c.height = tree.String(), src.MerklePath.BlockHeight
	c.outpoint = fmt.Sprintf("%s.%d", c.tree, in.SourceTxOutIndex)
	switch k, err := verify.Check(ctx, tx, headers); k {
	case verify.Passed:
	case verify.ProofRefused, verify.AncestryMissing:
		c.proof = refusal(verify.RefusedBump, "the funding transaction %s is not in the chain your header source serves: %v", c.tree, err)
		return c
	case verify.ScriptRefused:
		c.proof = refusal(verify.RefusedSig, "the input does not satisfy its funding output: %v", err)
		return c
	default:
		c.proof = &fail{err: fmt.Errorf("could not verify against the header source: %w", err)}
		return c
	}
	if o.Spends != nil {
		by, err := o.Spends.Spender(ctx, c.tree, in.SourceTxOutIndex)
		switch {
		case err != nil:
			c.retracted = &fail{err: fmt.Errorf("could not ask whether funding output %s is spent: %w", c.outpoint, err)}
		case by != "" && by != txid.String():
			c.retracted = refusal(RefusedRetracted, "funding output %s is spent by %s: the sender retracted this data", c.outpoint, by)
		}
	}
	return c
}

// memoTracker asks the header source about a root once, however many
// carriers rest on it and however many are being checked at the same time.
type memoTracker struct {
	t     chaintracker.ChainTracker
	mu    sync.Mutex
	roots map[rootKey]*rootAnswer
}

type rootKey struct {
	root   [32]byte
	height uint32
}

type rootAnswer struct {
	done chan struct{}
	ok   bool
	err  error
}

func (m *memoTracker) IsValidRootForHeight(ctx context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	if root == nil {
		return m.t.IsValidRootForHeight(ctx, root, height)
	}
	k := rootKey{root: *root, height: height}
	m.mu.Lock()
	a, asked := m.roots[k]
	if !asked {
		a = &rootAnswer{done: make(chan struct{})}
		m.roots[k] = a
	}
	m.mu.Unlock()
	if !asked {
		a.ok, a.err = m.t.IsValidRootForHeight(ctx, root, height)
		close(a.done)
	}
	select {
	case <-a.done:
		return a.ok, a.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (m *memoTracker) CurrentHeight(ctx context.Context) (uint32, error) {
	return m.t.CurrentHeight(ctx)
}

// frameLimits hold a frame to little more than a carrier needs: itself,
// the funding transaction, and that transaction's proof. The slack lets a
// carrier on unmined postage parse, so it is refused for what it is.
var frameLimits = func() guard.Limits {
	l := guard.DefaultLimits()
	l.Transactions, l.BUMPs = 4, 4
	return l
}()

func trusted(keys [][33]byte, id [33]byte) bool {
	for _, k := range keys {
		if k == id {
			return true
		}
	}
	return false
}
