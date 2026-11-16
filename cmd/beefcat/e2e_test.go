package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/testchain"

	"github.com/lightwebinc/beefcat/internal/home"
	"github.com/lightwebinc/beefcat/internal/peerpay"
	"github.com/lightwebinc/beefcat/send"
	"github.com/lightwebinc/beefcat/wire"
)

func init() { poll = 10 * time.Millisecond }

// cat runs one beefcat command and returns its exit status and streams.
func cat(t testing.TB, stdin []byte, args ...string) (int, []byte, string) {
	t.Helper()
	dir := t.TempDir()
	in, err := os.Create(filepath.Join(dir, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Write(stdin); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, _ := os.Create(filepath.Join(dir, "stdout"))
	errf, _ := os.Create(filepath.Join(dir, "stderr"))
	defer out.Close()
	defer errf.Close()
	code := run(context.Background(), args, in, out, errf)
	o, _ := os.ReadFile(out.Name())
	e, _ := os.ReadFile(errf.Name())
	return code, o, string(e)
}

// rig is a chain, a funded sender's home, and what a receiver is given.
type rig struct {
	t     testing.TB
	chain *testchain.Chain
	url   string
	home  string
	key   string
	// rx are a receiver's flags.
	rx []string
}

func newChain(t *testing.T) (*testchain.Chain, string) {
	t.Helper()
	c := testchain.New(700)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return c, srv.URL
}

func benchChain(b *testing.B) (*testchain.Chain, string) {
	b.Helper()
	c := testchain.New(700)
	srv := httptest.NewServer(c)
	b.Cleanup(srv.Close)
	return c, srv.URL
}

func newRig(t *testing.T, postage int) *rig {
	t.Helper()
	c, url := newChain(t)
	r := &rig{t: t, chain: c, url: url, rx: []string{"-network", "regtest", "-headers", url, "-chain", "asset:" + url}}
	r.home = r.newHome(postage)
	r.key = r.identity(r.home)
	return r
}

func (r *rig) newHome(postage int) string {
	r.t.Helper()
	h := filepath.Join(r.t.TempDir(), "home")
	if code, _, e := cat(r.t, nil, "init", "-home", h, "-devchain", r.url); code != 0 {
		r.t.Fatalf("init: %d\n%s", code, e)
	}
	if code, _, e := cat(r.t, nil, "fund", "-home", h, "-n", fmt.Sprint(postage), "-coinbase"); code != 0 {
		r.t.Fatalf("fund: %d\n%s", code, e)
	}
	return h
}

func (r *rig) identity(h string) string {
	r.t.Helper()
	_, o, _ := cat(r.t, nil, "status", "-home", h)
	for _, l := range strings.Split(string(o), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "identity" {
			return f[1]
		}
	}
	r.t.Fatalf("no identity in status:\n%s", o)
	return ""
}

// stream sends data to a file and returns the stream's bytes.
func (r *rig) stream(data []byte, flags ...string) []byte {
	r.t.Helper()
	f := filepath.Join(r.t.TempDir(), "stream")
	args := append([]string{"-home", r.home, "-o", f}, flags...)
	if code, _, e := cat(r.t, data, args...); code != 0 {
		r.t.Fatalf("send: %d\n%s", code, e)
	}
	b, err := os.ReadFile(f)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

// verify runs beefcat verify over a stream.
func (r *rig) verify(stream []byte, flags ...string) (int, []byte, string) {
	r.t.Helper()
	return cat(r.t, stream, append(append([]string{"verify"}, r.rx[:4]...), flags...)...)
}

// frames splits a stream into its frames, each without its length.
func frames(t *testing.T, stream []byte) [][]byte {
	t.Helper()
	rd := bytes.NewReader(stream)
	if err := wire.ReadPreamble(rd); err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for rd.Len() > 0 {
		f, err := wire.ReadFrame(rd)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func join(fs ...[]byte) []byte {
	var b bytes.Buffer
	b.Write(wire.Preamble)
	for _, f := range fs {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(f))) //nolint:gosec // a test frame
		b.Write(f)
	}
	return b.Bytes()
}

func random(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// refused asserts a loud refusal with the code, and returns standard output.
func refused(t *testing.T, want string, code int, out []byte, stderr string) []byte {
	t.Helper()
	wantExit := exitRefused
	if want == "REFUSED-UNTRUSTED" {
		wantExit = exitUntrusted
	}
	if code != wantExit {
		t.Fatalf("exit %d, want %d\n%s", code, wantExit, stderr)
	}
	if !strings.Contains(stderr, "beefcat: "+want+" carrier=") {
		t.Fatalf("no %s line:\n%s", want, stderr)
	}
	if !strings.Contains(stderr, "BEEFCAT REFUSED THIS DATA") || !strings.Contains(stderr, strings.Repeat("!", 76)) {
		t.Fatalf("the refusal is not loud:\n%s", stderr)
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	r := newRig(t, 16)
	for _, tc := range []struct {
		name  string
		data  []byte
		flags []string
		want  int // carriers
	}{
		{"empty", nil, nil, 1},
		{"one byte", []byte{7}, nil, 1},
		{"exact chunks", random(3000), []string{"-chunk", "1000"}, 3},
		{"ragged", random(3001), []string{"-chunk", "1000"}, 4},
		{"lines", []byte("one\ntwo\nthree"), []string{"-lines"}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := r.stream(tc.data, tc.flags...)
			if n := len(frames(t, s)); n != tc.want {
				t.Fatalf("%d carriers, want %d", n, tc.want)
			}
			code, out, e := r.verify(s, "-k", r.key)
			if code != 0 || !bytes.Equal(out, tc.data) {
				t.Fatalf("exit %d, %d bytes out of %d\n%s", code, len(out), len(tc.data), e)
			}
			if !strings.Contains(e, "VERIFIED") || strings.Contains(e, "!!!!") {
				t.Fatalf("a pinned, verified stream must be quiet:\n%s", e)
			}
		})
	}
}

func TestUnpinnedIsLoud(t *testing.T) {
	r := newRig(t, 2)
	data := random(500)
	code, out, e := r.verify(r.stream(data))
	if code != 0 || !bytes.Equal(out, data) {
		t.Fatalf("exit %d\n%s", code, e)
	}
	if !strings.Contains(e, "THE SENDER IS NOT PINNED") || !strings.Contains(e, "-k "+r.key) {
		t.Fatalf("an unpinned sender must be called out:\n%s", e)
	}
	// -q quiets the success report, never the warning.
	_, _, e = r.verify(r.stream(data), "-q")
	if !strings.Contains(e, "THE SENDER IS NOT PINNED") || strings.Contains(e, "VERIFIED") {
		t.Fatalf("-q:\n%s", e)
	}
}

func TestRefusals(t *testing.T) {
	r := newRig(t, 64)
	data := random(3000)
	good := r.stream(data, "-chunk", "1000")
	fs := frames(t, good)
	other := frames(t, r.stream(random(3000), "-chunk", "1000"))
	pin := []string{"-k", r.key}

	t.Run("untrusted sender", func(t *testing.T) {
		stranger := r.identity(r.newHome(1))
		code, out, e := r.verify(good, "-k", stranger)
		if out = refused(t, "REFUSED-UNTRUSTED", code, out, e); len(out) != 0 {
			t.Fatalf("%d bytes written for an untrusted sender", len(out))
		}
		if !strings.Contains(e, "UNTRUSTED SENDER") {
			t.Fatal(e)
		}
	})
	t.Run("altered in transit", func(t *testing.T) {
		// A flipped bit changes the carrier's txid, which the Atomic BEEF
		// names: the frame no longer holds the transaction it says it does.
		bad := bytes.Clone(fs[1])
		bad[len(bad)-300] ^= 1
		code, out, e := r.verify(join(fs[0], bad, fs[2]), pin...)
		out = refused(t, "REFUSED-DECODE", code, out, e)
		if !bytes.Equal(out, data[:1000]) {
			t.Fatalf("%d bytes written; only the first carrier verified", len(out))
		}
		if !strings.Contains(e, "CORRUPT OR ALTERED") || !strings.Contains(e, "THE OUTPUT IS INCOMPLETE") {
			t.Fatal(e)
		}
	})
	t.Run("altered and repackaged", func(t *testing.T) {
		// A forger who rebuilds the BEEF around altered data still has no
		// signature over it.
		_, tx, _, err := guard.ParseBEEF(fs[1], wire.MaxFrame)
		if err != nil {
			t.Fatal(err)
		}
		lock := *tx.Outputs[0].LockingScript
		lock[len(lock)-300] ^= 1
		bad, err := tx.AtomicBEEF(false)
		if err != nil {
			t.Fatal(err)
		}
		code, out, e := r.verify(join(fs[0], bad, fs[2]), pin...)
		out = refused(t, "REFUSED-SIG", code, out, e)
		if !bytes.Equal(out, data[:1000]) {
			t.Fatalf("%d bytes written; only the first carrier verified", len(out))
		}
		if !strings.Contains(e, "FORGED OR ALTERED DATA") {
			t.Fatal(e)
		}
	})
	t.Run("cut short", func(t *testing.T) {
		code, out, e := r.verify(join(fs[0], fs[1]), pin...)
		refused(t, "REFUSED-TRUNCATED", code, out, e)
		code, out, e = r.verify(good[:len(good)-10], pin...)
		refused(t, "REFUSED-TRUNCATED", code, out, e)
		code, out, e = r.verify(nil, pin...)
		refused(t, "REFUSED-DECODE", code, out, e)
	})
	t.Run("reordered", func(t *testing.T) {
		code, out, e := r.verify(join(fs[0], fs[2], fs[1]), pin...)
		refused(t, "REFUSED-SEQ", code, out, e)
	})
	t.Run("repeated", func(t *testing.T) {
		code, out, e := r.verify(join(fs[0], fs[0], fs[1], fs[2]), pin...)
		refused(t, "REFUSED-SEQ", code, out, e)
	})
	t.Run("spliced from another stream", func(t *testing.T) {
		code, out, e := r.verify(join(fs[0], other[1], fs[2]), pin...)
		refused(t, "REFUSED-SEQ", code, out, e)
	})
	t.Run("appended to", func(t *testing.T) {
		code, out, e := r.verify(append(bytes.Clone(good), 0), pin...)
		refused(t, "REFUSED-SEQ", code, out, e)
		code, out, e = r.verify(join(fs[0], fs[1], fs[2], other[0]), pin...)
		refused(t, "REFUSED-SEQ", code, out, e)
	})
	t.Run("mineable", func(t *testing.T) {
		_, tx, _, err := guard.ParseBEEF(fs[0], wire.MaxFrame)
		if err != nil {
			t.Fatal(err)
		}
		tx.LockTime = 0
		bad, err := tx.AtomicBEEF(false)
		if err != nil {
			t.Fatal(err)
		}
		code, out, e := r.verify(join(bad, fs[1], fs[2]), pin...)
		if out = refused(t, "REFUSED-MINEABLE", code, out, e); len(out) != 0 {
			t.Fatal("a mineable carrier's data was written")
		}
	})
	t.Run("not a stream", func(t *testing.T) {
		code, out, e := r.verify([]byte("GET / HTTP/1.1\r\n\r\n"), pin...)
		refused(t, "REFUSED-DECODE", code, out, e)
		code, out, e = r.verify(join([]byte("not a beef")), pin...)
		refused(t, "REFUSED-DECODE", code, out, e)
	})
	t.Run("postage on another chain", func(t *testing.T) {
		_, elsewhere := newChain(t)
		code, out, e := cat(t, good, "verify", "-network", "regtest", "-headers", elsewhere, "-k", r.key)
		if out = refused(t, "REFUSED-BUMP", code, out, e); len(out) != 0 {
			t.Fatal("data with unproven postage was written")
		}
		if !strings.Contains(e, "FRAUDULENT POSTAGE") {
			t.Fatal(e)
		}
	})
	t.Run("no header source is no verdict", func(t *testing.T) {
		code, out, e := cat(t, good, "verify", "-network", "regtest", "-headers", "http://127.0.0.1:1", "-k", r.key)
		if code != exitError || len(out) != 0 || !strings.Contains(e, "REACHED NO VERDICT") || strings.Contains(e, "REFUSED") {
			t.Fatalf("exit %d\n%s", code, e)
		}
	})
	t.Run("save keeps nothing refused", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "out")
		code, out, e := r.verify(join(fs[0], fs[1]), "-k", r.key, "-save", f)
		refused(t, "REFUSED-TRUNCATED", code, out, e)
		if _, err := os.Stat(f); err == nil || len(out) != 0 {
			t.Fatal("-save kept data of a refused stream")
		}
		if left, _ := filepath.Glob(f + "*"); len(left) != 0 {
			t.Fatalf("left behind: %v", left)
		}
		if code, _, e := r.verify(good, "-k", r.key, "-save", f); code != 0 {
			t.Fatal(e)
		}
		if got, _ := os.ReadFile(f); !bytes.Equal(got, data) {
			t.Fatal("-save wrote other bytes")
		}
	})
}

// An impostor signs with its own funded key and names the victim's
// identity in the payload.
func TestImpostor(t *testing.T) {
	r := newRig(t, 2)
	mallory := r.newHome(2)
	g := &global{home: mallory, cfg: home.Config{Network: "regtest"}}
	h, err := g.openHome()
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	sg := h.e.Signer()
	s := &send.Sender{Wallet: sg, Originator: sg.Originator, State: h.st}
	victim, err := parseKey(r.key)
	if err != nil {
		t.Fatal(err)
	}
	s.Identity = victim
	var b bytes.Buffer
	if _, err := s.Stream(context.Background(), strings.NewReader("pay mallory"), &b); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{{"-k", r.key}, nil} {
		code, out, e := r.verify(b.Bytes(), flags...)
		if out = refused(t, "REFUSED-KEY-DERIVE", code, out, e); len(out) != 0 {
			t.Fatal("an impostor's data was written")
		}
		if !strings.Contains(e, "FORGED SENDER IDENTITY") {
			t.Fatal(e)
		}
	}
}

func TestRetraction(t *testing.T) {
	r := newRig(t, 4)
	data := random(100)
	s := r.stream(data)
	if code, out, e := r.verify(s, "-k", r.key, "-unspent", "-chain", "asset:"+r.url); code != 0 || !bytes.Equal(out, data) || !strings.Contains(e, "every funding output unspent") {
		t.Fatalf("before the sweep: exit %d\n%s", code, e)
	}
	if code, o, e := cat(t, nil, "sweep", "-home", r.home); code != 0 || !strings.Contains(string(o), "nothing was spent") {
		t.Fatalf("sweep without -yes: %d\n%s%s", code, o, e)
	}
	if code, o, e := cat(t, nil, "sweep", "-home", r.home, "-yes"); code != 0 || !strings.Contains(string(o), "swept tree") {
		t.Fatalf("sweep: %d\n%s%s", code, o, e)
	}
	code, out, e := r.verify(s, "-k", r.key, "-unspent", "-chain", "asset:"+r.url)
	if out = refused(t, "REFUSED-RETRACTED", code, out, e); len(out) != 0 {
		t.Fatal("retracted data was written")
	}
	// SPV alone cannot see a retraction, and says it did not look.
	if code, _, e := r.verify(s, "-k", r.key); code != 0 || !strings.Contains(e, "not checked") {
		t.Fatalf("exit %d\n%s", code, e)
	}
	// The swept tree funds nothing more.
	if code, _, e := cat(t, []byte("x"), "-home", r.home, "-o", filepath.Join(t.TempDir(), "s")); code == 0 || !strings.Contains(e, "postage") {
		t.Fatalf("a send from a swept tree: %d\n%s", code, e)
	}
	if code, o, _ := cat(t, nil, "sweep", "-home", r.home, "-yes"); code != 0 || !strings.Contains(string(o), "nothing to sweep") {
		t.Fatalf("a second sweep: %d\n%s", code, o)
	}
}

func TestPostage(t *testing.T) {
	r := newRig(t, 2)
	// A file's size is known: refused before a carrier is spent.
	code, _, e := cat(t, random(3000), "-home", r.home, "-chunk", "1000", "-o", filepath.Join(t.TempDir(), "s"))
	if code == 0 || !strings.Contains(e, "takes 3 carrier(s) and the home has postage for 2") {
		t.Fatalf("exit %d\n%s", code, e)
	}
	if _, o, _ := cat(t, nil, "status", "-home", r.home); !strings.Contains(string(o), "postage      2 carrier(s) left") {
		t.Fatalf("postage was spent:\n%s", o)
	}
	// fund is a top-up: asking for what is there mints nothing.
	trees := func() int {
		_, o, _ := cat(t, nil, "status", "-home", r.home)
		return strings.Count(string(o), "\ntree ")
	}
	if code, _, e := cat(t, nil, "fund", "-home", r.home, "-n", "2"); code != 0 || trees() != 1 {
		t.Fatalf("fund -n 2 with 2 left: %d, %d trees\n%s", code, trees(), e)
	}
	if code, _, e := cat(t, nil, "fund", "-home", r.home, "-n", "5"); code != 0 || trees() != 2 {
		t.Fatalf("fund -n 5: %d, %d trees\n%s", code, trees(), e)
	}
	if _, o, _ := cat(t, nil, "status", "-home", r.home); !strings.Contains(string(o), "postage      5 carrier(s) left") {
		t.Fatalf("after the top-up:\n%s", o)
	}
	// A stream spans trees.
	data := random(4500)
	code, out, e := r.verify(r.stream(data, "-chunk", "1000"), "-k", r.key)
	if code != 0 || !bytes.Equal(out, data) {
		t.Fatalf("exit %d\n%s", code, e)
	}
}

func TestTCP(t *testing.T) {
	r := newRig(t, 8)
	port := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	}
	data := random(2500)

	t.Run("verified", func(t *testing.T) {
		p := port()
		done := listenOnce(t, r, p, "-k", r.key)
		code, e := dial(t, data, "-home", r.home, "-chunk", "1000", "127.0.0.1", p)
		if code != 0 || !strings.Contains(e, "the far end says: VERIFIED carriers=3 bytes=2500") {
			t.Fatalf("send: %d\n%s", code, e)
		}
		rc, out, re := done()
		if rc != 0 || !bytes.Equal(out, data) {
			t.Fatalf("receive: %d\n%s", rc, re)
		}
	})
	t.Run("the sender hears a refusal", func(t *testing.T) {
		p := port()
		stranger := r.identity(r.newHome(1))
		done := listenOnce(t, r, p, "-k", stranger)
		code, e := dial(t, data, "-home", r.home, "-chunk", "1000", "127.0.0.1", p)
		if code != exitUntrusted || !strings.Contains(e, "THE FAR END REFUSED THIS DATA") {
			t.Fatalf("send: %d\n%s", code, e)
		}
		rc, out, re := done()
		refused(t, "REFUSED-UNTRUSTED", rc, out, re)
	})
}

// dial sends, trying again while the listener's port is not open yet. A
// refused connection spends no postage.
func dial(t *testing.T, data []byte, args ...string) (int, string) {
	t.Helper()
	for i := 0; ; i++ {
		code, _, e := cat(t, data, args...)
		if i < 100 && code == exitError && strings.Contains(e, "connection refused") {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		return code, e
	}
}

// listenOnce starts a listener and returns a function that waits for it.
func listenOnce(t *testing.T, r *rig, port string, flags ...string) func() (int, []byte, string) {
	t.Helper()
	type res struct {
		code int
		out  []byte
		e    string
	}
	ch := make(chan res, 1)
	go func() {
		code, out, e := cat(t, nil, append(append([]string{"-l", "127.0.0.1:" + port}, r.rx[:4]...), flags...)...)
		ch <- res{code, out, e}
	}()
	return func() (int, []byte, string) {
		select {
		case x := <-ch:
			return x.code, x.out, x.e
		case <-time.After(20 * time.Second):
			t.Fatal("the listener did not finish")
			return 0, nil, ""
		}
	}
}

// forge signs carriers by hand, from the rig's own home, so a test can
// build what an honest sender never does.
type forge struct {
	t   *testing.T
	h   *opened
	id  [33]byte
	seq uint64
	sid [16]byte
}

func newForge(t *testing.T, r *rig) *forge {
	t.Helper()
	g := &global{home: r.home, cfg: home.Config{Network: "regtest"}}
	h, err := g.openHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.close)
	f := &forge{t: t, h: h}
	copy(f.id[:], h.e.Signer().IdentityKey().Compressed())
	return f
}

// carrier signs the next carrier of the stream on output vout of tree.
func (f *forge) carrier(tree *transaction.Transaction, vout uint32, data string, last bool) []byte {
	f.t.Helper()
	sg := f.h.e.Signer()
	hd := wire.Header{Last: last, Stream: f.sid, Seq: f.seq, Identity: f.id}
	tx, err := carrier.Mint(context.Background(), sg, sg.Originator, wire.Params, append(hd.Append(nil), data...), tree, vout)
	if err != nil {
		f.t.Fatal(err)
	}
	b, err := funding.BEEF(tx)
	if err != nil {
		f.t.Fatal(err)
	}
	f.seq++
	return b
}

// One funding output pays for one carrier: a sender that spends it twice
// has paid postage once.
func TestPostageSpentTwice(t *testing.T) {
	r := newRig(t, 2)
	f := newForge(t, r)
	rec := f.h.st.Trees[0]
	tree, err := funding.Rebuild(rec.RawHex, rec.BumpHex, "")
	if err != nil {
		t.Fatal(err)
	}
	s := join(f.carrier(tree, 0, "first", false), f.carrier(tree, 0, "second", true))
	code, out, e := r.verify(s, "-k", r.key)
	if out = refused(t, "REFUSED-FORK", code, out, e); string(out) != "first" {
		t.Fatalf("wrote %q", out)
	}
}

// Postage that was signed and never mined costs nothing, so it proves
// nothing: a funding tree with no Merkle path of its own is refused even
// though its ancestry verifies.
func TestPostageNeverMined(t *testing.T) {
	r := newRig(t, 1)
	f := newForge(t, r)
	g := &global{home: r.home, stderr: os.Stderr, quiet: true}
	if err := g.resolve(); err != nil {
		t.Fatal(err)
	}
	e, err := g.engine(context.Background(), f.h, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fee, err := e.payer.TakeAtLeast(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer e.payer.GiveBack()
	sg := f.h.e.Signer()
	lock, err := carrier.FundingLock(context.Background(), sg, sg.Originator, wire.Params)
	if err != nil {
		t.Fatal(err)
	}
	change, err := sg.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	tree, err := mint.FundingTree(lock, 1, 1, fee, change, mint.DefaultFees)
	if err != nil {
		t.Fatal(err)
	}
	if tree.MerklePath != nil {
		t.Fatal("the tree is mined")
	}
	s := join(f.carrier(tree, 0, "free postage", true))
	code, out, es := r.verify(s, "-k", r.key)
	if out = refused(t, "REFUSED-BUMP", code, out, es); len(out) != 0 {
		t.Fatal("data on unmined postage was written")
	}
	if !strings.Contains(es, "never mined") {
		t.Fatal(es)
	}
}

// A forged proof is a refusal, never "no verdict": a Merkle path borrowed
// from another transaction, and one claiming a block the chain does not
// have.
func TestForgedProof(t *testing.T) {
	r := newRig(t, 1)
	other := r.newHome(1)
	s := r.stream([]byte("data"))
	_, tx, _, err := guard.ParseBEEF(frames(t, s)[0], wire.MaxFrame)
	if err != nil {
		t.Fatal(err)
	}
	src := tx.Inputs[0].SourceTransaction
	honest := src.MerklePath

	t.Run("borrowed path", func(t *testing.T) {
		g := &global{home: other, cfg: home.Config{Network: "regtest"}}
		h, err := g.openHome()
		if err != nil {
			t.Fatal(err)
		}
		rec := h.st.Trees[0]
		h.close()
		foreign, err := funding.Rebuild(rec.RawHex, rec.BumpHex, "")
		if err != nil {
			t.Fatal(err)
		}
		src.MerklePath = foreign.MerklePath
		defer func() { src.MerklePath = honest }()
		bad, err := tx.AtomicBEEF(false)
		if err != nil {
			t.Skipf("the SDK does not write such a BEEF: %v", err)
		}
		code, out, e := r.verify(join(bad), "-k", r.key)
		if out = refused(t, "REFUSED-BUMP", code, out, e); len(out) != 0 {
			t.Fatal("data under a borrowed proof was written")
		}
	})
	t.Run("a block that does not exist", func(t *testing.T) {
		clone := *honest
		clone.BlockHeight = 4000000
		src.MerklePath = &clone
		defer func() { src.MerklePath = honest }()
		bad, err := tx.AtomicBEEF(false)
		if err != nil {
			t.Fatal(err)
		}
		code, out, e := r.verify(join(bad), "-k", r.key)
		if out = refused(t, "REFUSED-BUMP", code, out, e); len(out) != 0 {
			t.Fatal("data under a proof for a missing block was written")
		}
	})
}

// A frame is Atomic BEEF holding a carrier and what it spends, and is held
// to that before it is parsed: a BEEF of chained transactions costs its
// reader time that doubles with each one.
func TestHostileFrames(t *testing.T) {
	r := newRig(t, 1)
	t.Run("not atomic", func(t *testing.T) {
		code, out, e := r.verify(join(append([]byte{0x01, 0x00, 0xbe, 0xef}, make([]byte, 64)...)), "-k", r.key)
		refused(t, "REFUSED-DECODE", code, out, e)
		if !strings.Contains(e, "not Atomic BEEF") {
			t.Fatal(e)
		}
	})
	t.Run("a chain of transactions", func(t *testing.T) {
		lock := script.Script{script.OpTRUE}
		prev := transaction.NewTransaction()
		prev.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &lock})
		prev.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &lock})
		prev.MerklePath = transaction.NewMerklePath(800, [][]*transaction.PathElement{{{Offset: 0, Hash: prev.TxID(), Txid: boolPtr(true)}}})
		for i := 0; i < 10; i++ {
			tx := transaction.NewTransaction()
			tx.AddInputFromTx(prev, 0, nil)
			tx.AddInputFromTx(prev, 1, nil)
			for _, in := range tx.Inputs {
				in.UnlockingScript = &script.Script{}
			}
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &lock})
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &lock})
			prev = tx
		}
		bad, err := prev.AtomicBEEF(false)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		code, out, e := r.verify(join(bad), "-k", r.key)
		refused(t, "REFUSED-DECODE", code, out, e)
		if !strings.Contains(e, "the limit is") || time.Since(start) > 5*time.Second {
			t.Fatalf("after %s:\n%s", time.Since(start), e)
		}
	})
}

func boolPtr(b bool) *bool { return &b }

// A carrier is never mined, so one that arrives with a proof of its own
// carries a forged one, and that is a refusal, not "no verdict".
func TestCarrierWithProof(t *testing.T) {
	r := newRig(t, 1)
	_, tx, _, err := guard.ParseBEEF(frames(t, r.stream([]byte("data")))[0], wire.MaxFrame)
	if err != nil {
		t.Fatal(err)
	}
	tx.MerklePath = tx.Inputs[0].SourceTransaction.MerklePath
	bad, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Skipf("the SDK does not write such a BEEF: %v", err)
	}
	code, out, e := r.verify(join(bad), "-k", r.key)
	if out = refused(t, "REFUSED-BUMP", code, out, e); len(out) != 0 {
		t.Fatal("data was written")
	}
}

// Untrusted means genuine and not pinned: a stream that does not verify is
// refused for that, whoever is pinned.
func TestUntrustedMeansVerified(t *testing.T) {
	r := newRig(t, 1)
	_, elsewhere := newChain(t)
	stranger := "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	code, out, e := cat(t, r.stream([]byte("data")), "verify", "-network", "regtest", "-headers", elsewhere, "-k", stranger)
	refused(t, "REFUSED-BUMP", code, out, e)
}

// A refusal reaches the sender however large the stream, and stops it
// spending postage on a stream already refused. Carriers are signed a few
// ahead of the one being written, so a few more than were sent are spent.
func TestLargeRefusalReachesSender(t *testing.T) {
	r := newRig(t, 200)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	done := listenOnce(t, r, p, "-k", r.identity(r.newHome(1)))
	code, e := dial(t, random(200<<18), "-home", r.home, "-chunk", "256K", "127.0.0.1", p)
	if code != exitUntrusted || !strings.Contains(e, "THE FAR END REFUSED THIS DATA") {
		t.Fatalf("send: %d\n%s", code, e)
	}
	rc, out, re := done()
	refused(t, "REFUSED-UNTRUSTED", rc, out, re)
	if _, o, _ := cat(t, nil, "status", "-home", r.home); strings.Contains(string(o), "postage      0 carrier(s) left") {
		t.Fatalf("the whole stream was sent to a far end that had refused it:\n%s", o)
	}
}

func TestLinesRespectChunk(t *testing.T) {
	r := newRig(t, 8)
	data := []byte("a long line.\nxy\n")
	s := r.stream(data, "-lines", "-chunk", "4")
	for i, f := range frames(t, s) {
		_, tx, _, err := guard.ParseBEEF(f, wire.MaxFrame)
		if err != nil {
			t.Fatal(err)
		}
		c, err := carrier.Decode(tx, wire.Classify)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(c.Payload) - wire.HeaderLen; n > 4 {
			t.Fatalf("carrier %d holds %d bytes", i, n)
		}
	}
	if code, out, e := r.verify(s, "-k", r.key); code != 0 || !bytes.Equal(out, data) {
		t.Fatalf("%d %q\n%s", code, out, e)
	}
}

func TestSaveDoesNotOverwrite(t *testing.T) {
	r := newRig(t, 1)
	f := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(f, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, e := r.verify(r.stream([]byte("new")), "-k", r.key, "-save", f)
	if got, _ := os.ReadFile(f); code != exitUsage || string(got) != "old" {
		t.Fatalf("%d %q\n%s", code, got, e)
	}
}

func TestWithdrawToAddress(t *testing.T) {
	r := newRig(t, 1)
	key, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), false)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing is spent without -yes.
	code, o, e := cat(t, nil, "withdraw", "-home", r.home, "-sats", "5000", addr.AddressString)
	if code != 0 || !strings.Contains(string(o), "nothing was spent") {
		t.Fatalf("without -yes: %d\n%s%s", code, o, e)
	}
	code, o, e = cat(t, nil, "withdraw", "-home", r.home, "-sats", "5000", "-yes", addr.AddressString)
	if code != 0 || !strings.Contains(string(o), "mined at height") {
		t.Fatalf("withdraw: %d\n%s%s", code, o, e)
	}
	txid := strings.Fields(string(o)[strings.Index(string(o), "txid"):])[1]
	tx := r.chain.Tx(txid)
	want, _ := p2pkh.Lock(addr)
	if tx == nil || tx.Outputs[0].Satoshis != 5000 || !tx.Outputs[0].LockingScript.Equals(want) {
		t.Fatalf("the chain holds no payment of 5000 sat to %s in %s", addr.AddressString, txid)
	}
	// A mainnet address is refused on another network, and garbage is refused.
	for _, to := range []string{"1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2", "not-an-address"} {
		if code, _, _ := cat(t, nil, "withdraw", "-home", r.home, "-yes", to); code != exitUsage {
			t.Fatalf("withdraw to %q: exit %d", to, code)
		}
	}
}

// A payment to an identity key is one only that key's wallet can find and
// spend, and what reaches its inbox is what PeerPay reads: the token,
// encrypted to the recipient, under an id that is an HMAC of it.
func TestWithdrawToIdentityKey(t *testing.T) {
	r := newRig(t, 1)
	ctx := context.Background()
	rk, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := wallet.NewProtoWallet(wallet.ProtoWalletArgs{Type: wallet.ProtoWalletArgsTypePrivateKey, PrivateKey: rk})
	if err != nil {
		t.Fatal(err)
	}
	rhex := hex.EncodeToString(rk.PubKey().Compressed())
	var got []peerpay.Message
	var boxes []string
	fail := true
	old := deliver
	deliver = func(_ context.Context, box string, _ wallet.Interface, m peerpay.Message) error {
		if fail {
			return errors.New("the box is down")
		}
		got, boxes = append(got, m), append(boxes, box)
		return nil
	}
	defer func() { deliver = old }()

	// No message box: refused before anything is spent.
	if code, _, _ := cat(t, nil, "withdraw", "-home", r.home, "-yes", rhex); code != exitUsage {
		t.Fatalf("an identity key with no message box: exit %d", code)
	}
	// The box is down: the payment is made, loudly not delivered, and kept.
	link := "peerpay:" + rhex + "?url=https%3A%2F%2Fbox.example.com"
	code, o, e := cat(t, nil, "withdraw", "-home", r.home, "-sats", "4000", "-yes", link)
	if code == 0 || !strings.Contains(e, "THE PAYMENT IS MADE BUT NOT DELIVERED") {
		t.Fatalf("a failed delivery: %d\n%s%s", code, o, e)
	}
	txid := strings.Fields(string(o)[strings.Index(string(o), "txid"):])[1]
	if !strings.Contains(e, "beefcat withdraw -redeliver") || !strings.Contains(e, txid) {
		t.Fatalf("the failure does not say how to deliver again:\n%s", e)
	}
	fail = false
	if code, o, e := cat(t, nil, "withdraw", "-home", r.home, "-redeliver", txid); code != 0 || len(got) != 1 {
		t.Fatalf("redeliver: %d, %d message(s)\n%s%s", code, len(got), o, e)
	}
	m := got[0]
	if boxes[0] != "https://box.example.com" || m.Recipient != rhex {
		t.Fatalf("delivered to %s for %s", boxes[0], m.Recipient)
	}

	// The recipient opens it with nothing but its own key and the sender's
	// identity, which the message box supplies.
	sender, err := bwallet.Counterparty(r.key)
	if err != nil {
		t.Fatal(err)
	}
	args := wallet.EncryptionArgs{ProtocolID: peerpay.Protocol, KeyID: "1", Counterparty: sender}
	var body struct {
		EncryptedMessage string `json:"encryptedMessage"`
	}
	if err := json.Unmarshal([]byte(m.Body), &body); err != nil || body.EncryptedMessage == "" {
		t.Fatalf("body %q: %v", m.Body, err)
	}
	ct, err := base64.StdEncoding.DecodeString(body.EncryptedMessage)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := recipient.Decrypt(ctx, wallet.DecryptArgs{EncryptionArgs: args, Ciphertext: ct}, "test")
	if err != nil {
		t.Fatalf("the recipient cannot decrypt its payment: %v", err)
	}
	mac, err := recipient.CreateHMAC(ctx, wallet.CreateHMACArgs{EncryptionArgs: args, Data: peerpay.IDInput(plain.Plaintext)}, "test")
	if err != nil || hex.EncodeToString(mac.HMAC[:]) != m.ID {
		t.Fatalf("the message id is not the HMAC of the token: %v", err)
	}
	if in := peerpay.IDInput([]byte(`{"a":"b"}`)); string(in) != `"{\"a\":\"b\"}"` {
		t.Fatalf("the id's input is %s", in)
	}
	// The token is PeerPay's: the transaction a JSON array of numbers.
	if !bytes.Contains(plain.Plaintext, []byte(`"transaction":[1,1,1,1,`)) {
		t.Fatalf("the transaction is not a byte array of Atomic BEEF: %.80s", plain.Plaintext)
	}
	var tok peerpay.Token
	if err := json.Unmarshal(plain.Plaintext, &tok); err != nil {
		t.Fatal(err)
	}
	beef := make([]byte, len(tok.Transaction))
	for i, v := range tok.Transaction {
		beef[i] = byte(v)
	}
	_, tx, id, err := guard.ParseBEEF(beef, guard.DefaultBound)
	if err != nil || id.String() != txid || tok.Amount != 4000 || tx.Outputs[0].Satoshis != 4000 {
		t.Fatalf("token: txid %v, amount %d: %v", id, tok.Amount, err)
	}
	// Output 0 pays the key the recipient derives under BRC-29.
	forSelf := true
	pk, err := recipient.GetPublicKey(ctx, wallet.GetPublicKeyArgs{
		EncryptionArgs: wallet.EncryptionArgs{ProtocolID: bwallet.PaymentProtocol,
			KeyID: bwallet.PaymentKeyID(tok.CustomInstructions.DerivationPrefix, tok.CustomInstructions.DerivationSuffix), Counterparty: sender},
		ForSelf: &forSelf,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := script.NewAddressFromPublicKey(pk.PublicKey, false)
	want, _ := p2pkh.Lock(addr)
	if !tx.Outputs[0].LockingScript.Equals(want) {
		t.Fatal("output 0 is not locked to the key the recipient derives")
	}
	// And the payment proves itself: scripts, amounts and its parent's proof.
	hc, err := headers.NewSource(r.url)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := spv.Verify(ctx, tx, hc, nil); !ok || err != nil {
		t.Fatalf("the payment does not verify: %v", err)
	}
	if r.chain.Tx(txid) == nil {
		t.Fatal("the payment is not on the chain")
	}
}

func TestPeerPayLink(t *testing.T) {
	const key = "021d3359056f4f923a42b2b026a8fc2856b05320ee68f10233f9b8cbe76b14f8d2"
	k, box, err := peerpay.ParseURI("peerpay:" + key + "?url=https%3A%2F%2Fmessagebox.example.com")
	if err != nil || k != key || box != "https://messagebox.example.com" {
		t.Fatalf("%q %q %v", k, box, err)
	}
	if k, box, err := peerpay.ParseURI("peerpay:" + key); err != nil || k != key || box != "" {
		t.Fatalf("a link with no box: %q %q %v", k, box, err)
	}
	for _, bad := range []string{"peerpay:xyz", key, "peerpay:" + key + "?url=http%3A%2F%2Fplain.example.com", "peerpay:" + key + "?url=ftp%3A%2F%2Fx"} {
		if _, _, err := peerpay.ParseURI(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}

// A key has one spelling, and a notice is delivered again only to a message
// box a new withdrawal could have used.
func TestWithdrawGuards(t *testing.T) {
	r := newRig(t, 1)
	if err := peerpay.CheckKey(strings.ToUpper(r.key)); err == nil {
		t.Fatal("an uppercase key was accepted")
	}
	// The home's own key, in uppercase, is refused as a key, not sent to.
	if code, _, _ := cat(t, nil, "withdraw", "-home", r.home, "-messagebox", "https://box.example.com", "-yes", strings.ToUpper(r.key)); code != exitUsage {
		t.Fatalf("the home's own key in uppercase: exit %d", code)
	}
	rk, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	old := deliver
	var boxes []string
	deliver = func(_ context.Context, box string, _ wallet.Interface, _ peerpay.Message) error {
		boxes = append(boxes, box)
		return nil
	}
	defer func() { deliver = old }()
	link := "peerpay:" + hex.EncodeToString(rk.PubKey().Compressed()) + "?url=https%3A%2F%2Fbox.example.com"
	code, o, e := cat(t, nil, "withdraw", "-home", r.home, "-sats", "1000", "-yes", link)
	if code != 0 {
		t.Fatalf("withdraw: %d\n%s%s", code, o, e)
	}
	txid := strings.Fields(string(o)[strings.Index(string(o), "txid"):])[1]
	if code, _, _ := cat(t, nil, "withdraw", "-home", r.home, "-redeliver", txid, "-messagebox", "http://box.example.com"); code != exitUsage || len(boxes) != 1 {
		t.Fatalf("redelivery to a plain http box: exit %d, %d deliveries", code, len(boxes))
	}
	if code, _, _ := cat(t, nil, "withdraw", "-home", r.home, "-redeliver", txid, "-messagebox", "https://other.example.com"); code != 0 || boxes[len(boxes)-1] != "https://other.example.com" {
		t.Fatalf("redelivery to another https box: exit %d, %v", code, boxes)
	}
}

// failAfter is a writer that takes n writes and then fails.
type failAfter struct{ n int }

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n--; f.n < 0 {
		return 0, errors.New("the far end went away")
	}
	return len(p), nil
}

// Once a send has returned, input that arrives later spends no postage:
// the reader it left behind cannot be called off, but it changes nothing.
func TestNoPostageSpentAfterSendReturns(t *testing.T) {
	r := newRig(t, 8)
	g := &global{home: r.home, cfg: home.Config{Network: "regtest"}}
	h, err := g.openHome()
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	sg := h.e.Signer()
	s := &send.Sender{Wallet: sg, Originator: sg.Originator, State: h.st, Lines: true}
	copy(s.Identity[:], sg.IdentityKey().Compressed())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	// The preamble and the first line's frame (length, body) are taken;
	// the second line's is not.
	go func() { _, err := s.Stream(context.Background(), pr, &failAfter{n: 3}); done <- err }()
	_, _ = pw.Write([]byte("one\n"))
	_, _ = pw.Write([]byte("two\n"))
	if err := <-done; err == nil {
		t.Fatal("the send did not fail")
	}
	left := h.st.Remaining()
	_, _ = pw.Write([]byte("three\nfour\n"))
	pw.Close()
	time.Sleep(100 * time.Millisecond)
	if got := h.st.Remaining(); got != left {
		t.Fatalf("postage went from %d to %d after the send returned", left, got)
	}
}
