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
//     sequence, naming the carrier before it;
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
func Receive(ctx context.Context, r io.Reader, w io.Writer, o Options) (Summary, error) {
	var s Summary
	if o.Headers == nil {
		return s, verify.ErrNoTracker
	}
	refuse := func(code verify.Code, format string, args ...any) (Summary, error) {
		return s, &Refusal{Code: code, Reason: fmt.Sprintf(format, args...), Carrier: s.Carriers}
	}
	if err := wire.ReadPreamble(r); err != nil {
		return refuse(verify.RefusedDecode, "%v", err)
	}
	var prev [32]byte
	spent := map[string]bool{}
	for {
		frame, err := wire.ReadFrame(r)
		switch {
		case err == io.EOF:
			return refuse(RefusedTruncated, "the stream ended after %d carrier(s) with no signed last carrier: it was cut short", s.Carriers)
		case errors.Is(err, io.ErrUnexpectedEOF):
			return refuse(RefusedTruncated, "the stream ended inside a frame: it was cut short")
		case errors.Is(err, wire.ErrFrame):
			return refuse(verify.RefusedDecode, "%v", err)
		case err != nil:
			return s, fmt.Errorf("reading carrier %d: %w", s.Carriers, err)
		}
		// A frame is Atomic BEEF and nothing else, holding a carrier and the
		// transaction it spends. Both are checked before the SDK parses a
		// byte: a BEEF of many chained transactions costs its reader time
		// that doubles with each one.
		if !wire.IsAtomic(frame) {
			return refuse(verify.RefusedDecode, "the frame is not Atomic BEEF")
		}
		_, tx, txid, err := guard.ParseBEEFWithin(frame, wire.MaxFrame, frameLimits)
		if err != nil || tx == nil || txid == nil {
			return refuse(verify.RefusedDecode, "BEEF does not parse: %v", err)
		}
		if tx.MerklePath != nil {
			return refuse(verify.RefusedBump, "the carrier comes with a Merkle path of its own: a carrier is never mined, so the proof is forged")
		}
		c, err := carrier.Decode(tx, wire.Classify)
		if err != nil {
			return refuse(verify.RefusedDecode, "not a carrier: %v", err)
		}
		h, data, err := wire.Parse(c.Payload)
		if err != nil {
			return refuse(verify.RefusedDecode, "%v", err)
		}
		if err := c.Validate(wire.Params, h.Identity[:]); err != nil {
			switch {
			case errors.Is(err, carrier.ErrMineable):
				return refuse(verify.RefusedMineable, "%v", err)
			case errors.Is(err, carrier.ErrUnlocking):
				return refuse(verify.RefusedUnlocking, "%v", err)
			case errors.Is(err, carrier.ErrLock):
				return refuse(verify.RefusedKeyDerive, "%v: the carrier is not locked by the identity it names", err)
			case errors.Is(err, carrier.ErrSignature):
				return refuse(verify.RefusedSig, "%v: the data is not what the sender signed", err)
			default:
				return refuse(verify.RefusedDecode, "%v", err)
			}
		}
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
		case h.Prev != prev:
			return refuse(verify.RefusedSeq, "does not name the carrier before it: the stream was spliced")
		}
		in := tx.Inputs[0]
		src := in.SourceTransaction
		if src == nil || uint64(in.SourceTxOutIndex) >= uint64(len(src.Outputs)) {
			return refuse(verify.RefusedBump, "the funding transaction is not in the BEEF")
		}
		if src.MerklePath == nil {
			return refuse(verify.RefusedBump, "the funding transaction %s carries no Merkle path: its postage was never mined", src.TxID())
		}
		key, ok := carrier.DecodeFunding(src.Outputs[in.SourceTxOutIndex].LockingScript, wire.FundingTag)
		if !ok || !key.IsEqual(c.LockingKey) {
			return refuse(verify.RefusedDecode, "the input does not spend a beefcat funding output of the sender's")
		}
		// A path that does not lead from the funding transaction to a root
		// is a forged proof, and is refused here by arithmetic. Left to
		// SPV it would surface as an error with no verdict, as if the
		// header source had failed.
		if _, err := src.MerklePath.ComputeRoot(src.TxID()); err != nil {
			return refuse(verify.RefusedBump, "the Merkle path given for the funding transaction %s does not lead from it: %v", src.TxID(), err)
		}
		outpoint := fmt.Sprintf("%s.%d", src.TxID(), in.SourceTxOutIndex)
		if spent[outpoint] {
			return refuse(verify.RefusedFork, "funding output %s is spent by an earlier carrier of this stream", outpoint)
		}
		switch k, err := verify.Check(ctx, tx, o.Headers); k {
		case verify.Passed:
		case verify.ProofRefused, verify.AncestryMissing:
			return refuse(verify.RefusedBump, "the funding transaction %s is not in the chain your header source serves: %v", src.TxID(), err)
		case verify.ScriptRefused:
			return refuse(verify.RefusedSig, "the input does not satisfy its funding output: %v", err)
		default:
			return s, fmt.Errorf("carrier %d: could not verify against the header source: %w", s.Carriers, err)
		}
		if o.Spends != nil {
			by, err := o.Spends.Spender(ctx, src.TxID().String(), in.SourceTxOutIndex)
			if err != nil {
				return s, fmt.Errorf("carrier %d: could not ask whether funding output %s is spent: %w", s.Carriers, outpoint, err)
			}
			if by != "" && by != txid.String() {
				return refuse(RefusedRetracted, "funding output %s is spent by %s: the sender retracted this data", outpoint, by)
			}
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
		if _, err := w.Write(data); err != nil {
			return s, fmt.Errorf("writing carrier %d: %w", s.Carriers, err)
		}
		spent[outpoint] = true
		prev = carrier.Commitment(tx)
		s.Carriers++
		s.Bytes += int64(len(data))
		s.Tree, s.Height = src.TxID().String(), src.MerklePath.BlockHeight
		s.Unspent = o.Spends != nil
		if h.Last {
			break
		}
	}
	// Anything after the signed last carrier is not part of the stream.
	var one [1]byte
	if n, err := io.ReadFull(r, one[:]); n > 0 {
		return refuse(verify.RefusedSeq, "bytes follow the signed last carrier: something was appended to the stream")
	} else if err != io.EOF {
		return s, fmt.Errorf("reading the end of the stream: %w", err)
	}
	return s, nil
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
