// Package seen is a receiver's memory of the funding outputs it has
// accepted carriers on: an append-only file of fixed records, read into a
// map when it is opened.
//
// A carrier is never mined, so the chain keeps no record of a funding
// output having been used, and a stream that verified once verifies again.
// A receiver that records each output as it accepts a carrier on it, before
// it writes that carrier's data, can refuse the output the second time. A
// crash between the record and the write loses data and never opens a
// replay.
//
// The file grows by one record per carrier. Prune drops the records of
// outputs mined below a height, and keeps that height as a floor: a
// receiver that also refuses postage older than a limit needs no memory of
// outputs older than that, so long as it takes nothing from below the
// floor again.
package seen

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// The file: an 8-byte magic, then records of RecordLen bytes.
//
//	offset  size  field
//	0       32    funding transaction id
//	32      4     funding output index, big-endian
//	36      32    id of the carrier accepted on that output
//	68      4     height the funding transaction was mined at, big-endian
//
// A record whose first 36 bytes are zero names no output: its height is
// the floor.
const (
	RecordLen = 72
	magic     = "BCSEEN\x01\n"
)

// Outpoint names a funding output: a transaction id and an output index,
// big-endian.
type Outpoint [36]byte

// NewOutpoint builds an Outpoint.
func NewOutpoint(txid [32]byte, vout uint32) Outpoint {
	var o Outpoint
	copy(o[:32], txid[:])
	binary.BigEndian.PutUint32(o[32:], vout)
	return o
}

type entry struct {
	carrier [32]byte
	height  uint32
}

// ErrFormat is a file that is not a seen-output log.
var ErrFormat = errors.New("seen: not a beefcat seen-output file")

// Log is an open seen-output file. One process holds it at a time. It is
// not safe for use by several goroutines.
type Log struct {
	path   string
	f      *os.File
	w      *bufio.Writer
	m      map[Outpoint]entry
	floor  uint32
	dirty  bool
	unlock func()
}

// Open opens the file at path, creating it when it is not there, and takes
// its lock (path with ".lock" appended). A record cut short at the end of
// the file, left by a crash, is dropped.
func Open(path string) (*Log, error) {
	unlock, err := lock(path + ".lock")
	if err != nil {
		return nil, err
	}
	l, err := open(path)
	if err != nil {
		unlock()
		return nil, err
	}
	l.unlock = unlock
	return l, nil
}

func open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	l := &Log{path: path, f: f, m: map[Outpoint]entry{}}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size() < int64(len(magic)) {
		// New, or a crash before the magic was whole: nothing was recorded.
		// A short file of anything else is not ours to replace.
		head := make([]byte, st.Size())
		if _, err := io.ReadFull(f, head); err != nil || !bytes.HasPrefix([]byte(magic), head) {
			f.Close()
			return nil, fmt.Errorf("%w: %s", ErrFormat, path)
		}
		if err := l.reset(nil, 0); err != nil {
			f.Close()
			return nil, err
		}
		return l, nil
	}
	br := bufio.NewReaderSize(f, 1<<20)
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(br, head); err != nil || !bytes.Equal(head, []byte(magic)) {
		f.Close()
		return nil, fmt.Errorf("%w: %s", ErrFormat, path)
	}
	var rec [RecordLen]byte
	whole := int64(len(magic))
	for {
		if _, err := io.ReadFull(br, rec[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			f.Close()
			return nil, err
		}
		var o Outpoint
		var e entry
		copy(o[:], rec[:36])
		copy(e.carrier[:], rec[36:68])
		e.height = binary.BigEndian.Uint32(rec[68:])
		// The first record of an output stands: it is the carrier that
		// was accepted. The record of no output holds the floor.
		if o == (Outpoint{}) {
			l.floor = max(l.floor, e.height)
		} else if _, ok := l.m[o]; !ok {
			l.m[o] = e
		}
		whole += RecordLen
	}
	if whole != st.Size() {
		if err := f.Truncate(whole); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
	}
	if _, err := f.Seek(whole, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	l.w = bufio.NewWriterSize(f, 64<<10)
	return l, nil
}

// reset makes the file hold exactly the floor and the records of keep,
// durably. Both files are closed before the one replaces the other, which
// some platforms require, and the log is opened again after. If the
// replacement fails the old file is opened again and nothing is lost.
func (l *Log) reset(keep map[Outpoint]entry, floor uint32) error {
	tmp, err := os.CreateTemp(filepath.Dir(l.path), filepath.Base(l.path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	bw := bufio.NewWriterSize(tmp, 1<<20)
	_, _ = bw.WriteString(magic)
	if floor > 0 {
		_, _ = bw.Write(record(Outpoint{}, entry{height: floor}))
	}
	for o, e := range keep {
		_, _ = bw.Write(record(o, e))
	}
	err = bw.Flush()
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	rerr := os.Rename(tmp.Name(), l.path)
	if rerr == nil {
		syncDir(filepath.Dir(l.path))
	}
	f, err := os.OpenFile(l.path, os.O_RDWR, 0o600)
	if err != nil {
		return errors.Join(rerr, err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return errors.Join(rerr, err)
	}
	l.f, l.w, l.dirty = f, bufio.NewWriterSize(f, 64<<10), false
	if rerr != nil {
		return rerr
	}
	if keep == nil {
		keep = map[Outpoint]entry{}
	}
	l.m, l.floor = keep, floor
	return nil
}

// syncDir makes a rename in dir durable where the platform can.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

func record(o Outpoint, e entry) []byte {
	var rec [RecordLen]byte
	copy(rec[:36], o[:])
	copy(rec[36:68], e.carrier[:])
	binary.BigEndian.PutUint32(rec[68:], e.height)
	return rec[:]
}

// Len is how many outputs are recorded.
func (l *Log) Len() int { return len(l.m) }

// Lookup reports the carrier recorded on an output, if any.
func (l *Log) Lookup(o Outpoint) (carrier [32]byte, ok bool) {
	e, ok := l.m[o]
	return e.carrier, ok
}

// Add records a carrier accepted on an output mined at height. The record
// is durable only after Sync. An output already recorded keeps its first
// carrier.
func (l *Log) Add(o Outpoint, carrier [32]byte, height uint32) error {
	if _, ok := l.m[o]; ok {
		return nil
	}
	e := entry{carrier: carrier, height: height}
	if _, err := l.w.Write(record(o, e)); err != nil {
		return err
	}
	l.m[o] = e
	l.dirty = true
	return nil
}

// Sync makes every record added so far durable.
func (l *Log) Sync() error {
	if !l.dirty {
		return nil
	}
	if err := l.w.Flush(); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.dirty = false
	return nil
}

// Floor is the height below which the log no longer holds records: the
// highest height it was ever pruned to. An output mined below it may have
// been seen and forgotten, so a reader does not take one.
func (l *Log) Floor() uint32 { return l.floor }

// Prune drops the records of outputs mined below height, raises the floor
// to height, and returns how many records it dropped. The floor is kept in
// the file and never falls, so a later prune to a lower height, as after
// a header source reports a lower tip, forgets nothing and admits nothing
// that was forgotten. The file is rewritten whole and replaced.
func (l *Log) Prune(height uint32) (int, error) {
	if err := l.Sync(); err != nil {
		return 0, err
	}
	if height <= l.floor {
		return 0, nil
	}
	keep := make(map[Outpoint]entry, len(l.m))
	for o, e := range l.m {
		if e.height >= height {
			keep[o] = e
		}
	}
	return len(l.m) - len(keep), l.reset(keep, height)
}

// Close syncs and releases the file and its lock.
func (l *Log) Close() error {
	err := l.Sync()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	if l.unlock != nil {
		l.unlock()
	}
	return err
}
