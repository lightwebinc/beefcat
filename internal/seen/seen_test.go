package seen

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func op(n byte, vout uint32) Outpoint { return NewOutpoint([32]byte{n}, vout) }

func mustOpen(t *testing.T, path string) *Log {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRecordAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")
	l := mustOpen(t, path)
	if _, ok := l.Lookup(op(1, 0)); ok {
		t.Fatal("an empty log has a record")
	}
	if err := l.Add(op(1, 0), [32]byte{0xaa}, 700); err != nil {
		t.Fatal(err)
	}
	// The first carrier of an output stands.
	if err := l.Add(op(1, 0), [32]byte{0xbb}, 700); err != nil {
		t.Fatal(err)
	}
	if c, ok := l.Lookup(op(1, 0)); !ok || c != ([32]byte{0xaa}) {
		t.Fatalf("lookup: %x %v", c[:1], ok)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l = mustOpen(t, path)
	defer l.Close()
	if c, ok := l.Lookup(op(1, 0)); !ok || c != ([32]byte{0xaa}) || l.Len() != 1 {
		t.Fatalf("after reopening: %x %v, %d record(s)", c[:1], ok, l.Len())
	}
	if _, ok := l.Lookup(op(1, 1)); ok {
		t.Fatal("another output of the tree reads as seen")
	}
}

// A process that dies after Sync, without Close, loses nothing; one that
// dies before Sync may lose what it added, and never anything older.
func TestCrashOrdering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")
	l := mustOpen(t, path)
	_ = l.Add(op(1, 0), [32]byte{1}, 700)
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = l.Add(op(2, 0), [32]byte{2}, 700) // never synced: still in the buffer
	// The crash: the descriptor and the lock go, the buffer is lost.
	l.f.Close()
	l.unlock()
	l = mustOpen(t, path)
	defer l.Close()
	if _, ok := l.Lookup(op(1, 0)); !ok {
		t.Fatal("a synced record was lost")
	}
	if _, ok := l.Lookup(op(2, 0)); ok {
		t.Fatal("a record that was never written reads as seen")
	}
}

func TestTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")
	l := mustOpen(t, path)
	_ = l.Add(op(1, 0), [32]byte{1}, 700)
	_ = l.Add(op(2, 0), [32]byte{2}, 701)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, RecordLen-5)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	l = mustOpen(t, path)
	if l.Len() != 2 {
		t.Fatalf("%d record(s), want 2", l.Len())
	}
	// The torn record is gone, so the next one lands on a boundary.
	_ = l.Add(op(3, 0), [32]byte{3}, 702)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if want := int64(len(magic) + 3*RecordLen); st.Size() != want {
		t.Fatalf("file is %d bytes, want %d", st.Size(), want)
	}
	l = mustOpen(t, path)
	defer l.Close()
	if _, ok := l.Lookup(op(3, 0)); !ok || l.Len() != 3 {
		t.Fatalf("after the torn tail: %d record(s)", l.Len())
	}
}

func TestPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")
	l := mustOpen(t, path)
	for i := byte(0); i < 10; i++ {
		_ = l.Add(op(i, 0), [32]byte{i}, 700+uint32(i))
	}
	n, err := l.Prune(705)
	if err != nil || n != 5 || l.Len() != 5 {
		t.Fatalf("pruned %d, %d left: %v", n, l.Len(), err)
	}
	if _, ok := l.Lookup(op(4, 0)); ok {
		t.Fatal("a pruned record reads as seen")
	}
	// The log still takes records after it was replaced.
	_ = l.Add(op(20, 0), [32]byte{20}, 720)
	if n, err := l.Prune(0); err != nil || n != 0 {
		t.Fatalf("a prune of nothing: %d %v", n, err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l = mustOpen(t, path)
	defer l.Close()
	if l.Len() != 6 {
		t.Fatalf("%d record(s) after reopening, want 6", l.Len())
	}
	for _, i := range []byte{5, 9, 20} {
		if _, ok := l.Lookup(op(i, 0)); !ok {
			t.Fatalf("record %d was lost", i)
		}
	}
	if left, _ := filepath.Glob(path + ".*"); len(left) != 1 { // the lock file
		t.Fatalf("files left beside the log: %v", left)
	}
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")
	l := mustOpen(t, path)
	if _, err := Open(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second open: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l = mustOpen(t, path)
	l.Close()
}

func TestNotALog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes")
	if err := os.WriteFile(path, []byte("these are my notes, not a log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrFormat) {
		t.Fatalf("a file of something else: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "these are my notes, not a log\n" {
		t.Fatal("the file was changed")
	}
	// The lock was released.
	if _, err := Open(path); !errors.Is(err, ErrFormat) {
		t.Fatalf("a second open: %v", err)
	}
}
