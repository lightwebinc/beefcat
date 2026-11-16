// Package wire is everything beefcat freezes at its first release: the key
// derivation, the funding tag, the signed payload header every carrier
// holds, and the framing of a stream of carriers (docs/frozen.md).
//
// A stream is a preamble followed by frames. Each frame is one carrier in
// Atomic BEEF (BRC-95): an unmineable transaction whose output holds a
// signed payload and whose one input spends a mined funding output
// (bcommon's carrier package). The payload is a fixed header followed by the
// sender's bytes. The header chains every carrier to the one before it, so
// order and the end of the stream are signed and a reader can refuse a
// stream that was reordered, spliced or cut short.
package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// The derivation and the funding tag. Both are hashed into or matched
// against every key and funding output, so they never change.
const (
	ProtocolName = "beefcat carrier"
	KeyCarrier   = "1"
	KeyFund      = "fund"
)

var (
	// Protocol is the BRC-43 protocol every beefcat key derives under.
	Protocol = wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: ProtocolName}
	// Derivation locks carriers and funding outputs.
	Derivation = pushdrop.Derivation{Protocol: Protocol, KeyID: KeyCarrier}
	// FundingTag is the one field of a funding output.
	FundingTag = []byte("beefcat fund")
	// Profile is the embedded wallet's profile.
	Profile = bwallet.Profile{
		FundProtocol:   Protocol,
		FundKeyID:      KeyFund,
		FundBasket:     "beefcat fund",
		Version:        "beefcat-1",
		LegacyPoolFile: "beefcat-pool.json",
	}
)

// The payload header, 95 bytes, every integer big-endian:
//
//	offset  size  field
//	0       4     magic "BCAT"
//	4       1     version, 1
//	5       1     flags; bit 0 set on the last carrier of a stream
//	6       16    stream id, random per stream
//	22      8     sequence, from 0
//	30      32    commitment (txid, hash byte order) of the previous
//	              carrier; all zero when the sequence is 0
//	62      33    the sender's identity key, compressed
//	95      ...   data, 0 to MaxData bytes
const (
	Magic     = "BCAT"
	Version   = 1
	FlagLast  = 0x01
	HeaderLen = 95
	// MaxData is the most data one carrier holds.
	MaxData = 4 << 20
	// MaxFrame bounds one frame: the data, its header and transaction, and
	// the largest funding tree with its proof.
	MaxFrame = MaxData + 64<<10
)

// Preamble opens every stream: seven letters and the framing version.
var Preamble = []byte("BEEFCAT\x01")

// Header is a decoded payload header.
type Header struct {
	Last     bool
	Stream   [16]byte
	Seq      uint64
	Prev     [32]byte
	Identity [33]byte
}

// The payload refusals. Their texts are what readers print.
var (
	ErrPayload = errors.New("beefcat: payload is not a beefcat record")
	ErrVersion = errors.New("beefcat: payload version is not 1")
)

// Append appends the header to dst.
func (h Header) Append(dst []byte) []byte {
	dst = append(dst, Magic...)
	flags := byte(0)
	if h.Last {
		flags = FlagLast
	}
	dst = append(dst, Version, flags)
	dst = append(dst, h.Stream[:]...)
	dst = binary.BigEndian.AppendUint64(dst, h.Seq)
	dst = append(dst, h.Prev[:]...)
	return append(dst, h.Identity[:]...)
}

// Parse reads a payload's header and returns it with the data that follows.
// It applies the payload's own rules, in a fixed order, so a payload that
// breaks two is refused for the first.
func Parse(payload []byte) (Header, []byte, error) {
	var h Header
	if len(payload) < HeaderLen || string(payload[:4]) != Magic {
		return h, nil, ErrPayload
	}
	if payload[4] != Version {
		return h, nil, fmt.Errorf("%w (it is %d)", ErrVersion, payload[4])
	}
	if payload[5]&^FlagLast != 0 {
		return h, nil, fmt.Errorf("%w: unknown flags 0x%02x", ErrPayload, payload[5])
	}
	h.Last = payload[5]&FlagLast != 0
	copy(h.Stream[:], payload[6:22])
	h.Seq = binary.BigEndian.Uint64(payload[22:30])
	copy(h.Prev[:], payload[30:62])
	copy(h.Identity[:], payload[62:95])
	if (h.Seq == 0) != (h.Prev == [32]byte{}) {
		return h, nil, fmt.Errorf("%w: the first carrier, and only the first, names no previous carrier", ErrPayload)
	}
	data := payload[HeaderLen:]
	if len(data) > MaxData {
		return h, nil, fmt.Errorf("%w: %d bytes of data, more than %d", ErrPayload, len(data), MaxData)
	}
	return h, data, nil
}

// Classify takes a PushDrop field that opens with the magic as a beefcat
// payload.
func Classify(payload []byte) (bool, error) {
	return bytes.HasPrefix(payload, []byte(Magic)), nil
}

// Params are the carrier parameters of every beefcat carrier.
var Params = carrier.Params{
	Derivation: Derivation,
	FundingTag: FundingTag,
	ValidatePayload: func(p []byte) error {
		_, _, err := Parse(p)
		return err
	},
}

// ErrFrame is a frame a reader does not take.
var ErrFrame = errors.New("beefcat: bad frame")

// IsAtomic reports whether a frame opens as Atomic BEEF (BRC-95): the
// prefix 0x01010101 and the subject's transaction id.
func IsAtomic(frame []byte) bool {
	return len(frame) > 36 && bytes.Equal(frame[:4], []byte{1, 1, 1, 1})
}

// WritePreamble opens a stream.
func WritePreamble(w io.Writer) error {
	_, err := w.Write(Preamble)
	return err
}

// ReadPreamble reads and checks a stream's opening.
func ReadPreamble(r io.Reader) error {
	b := make([]byte, len(Preamble))
	if _, err := io.ReadFull(r, b); err != nil {
		if err == io.EOF {
			return fmt.Errorf("%w: empty stream", ErrFrame)
		}
		return fmt.Errorf("%w: stream opening: %v", ErrFrame, err)
	}
	if !bytes.Equal(b, Preamble) {
		return fmt.Errorf("%w: not a beefcat stream (it opens %q)", ErrFrame, b)
	}
	return nil
}

// WriteFrame writes one carrier's BEEF as a frame: a four-byte length and
// the bytes.
func WriteFrame(w io.Writer, beef []byte) error {
	if len(beef) == 0 || len(beef) > MaxFrame {
		return fmt.Errorf("%w: %d bytes", ErrFrame, len(beef))
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(beef))) //nolint:gosec // bounded above
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err := w.Write(beef)
	return err
}

// ReadFrame reads one frame. It answers io.EOF only at a clean frame
// boundary; a stream that ends inside a frame is io.ErrUnexpectedEOF, and a
// length out of bounds is ErrFrame, before anything is allocated for it.
func ReadFrame(r io.Reader) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 || size > MaxFrame {
		return nil, fmt.Errorf("%w: length %d, outside 1 to %d", ErrFrame, size, MaxFrame)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return b, nil
}
