package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	h := Header{Last: true, Seq: 7}
	h.Stream[0], h.Prev[31], h.Identity[0] = 1, 2, 3
	p := append(h.Append(nil), "data"...)
	if len(p) != HeaderLen+4 {
		t.Fatalf("header is %d bytes, want %d", len(p)-4, HeaderLen)
	}
	got, data, err := Parse(p)
	if err != nil || got != h || string(data) != "data" {
		t.Fatalf("%+v %q %v", got, data, err)
	}
	if ours, _ := Classify(p); !ours {
		t.Fatal("not classified")
	}
	if ours, _ := Classify([]byte("other")); ours {
		t.Fatal("classified another payload")
	}
}

func TestParseRefusals(t *testing.T) {
	good := func() []byte { return Header{}.Append(nil) }
	for name, mut := range map[string]func([]byte) []byte{
		"short":            func(p []byte) []byte { return p[:HeaderLen-1] },
		"magic":            func(p []byte) []byte { p[0] = 'X'; return p },
		"version":          func(p []byte) []byte { p[4] = 2; return p },
		"flags":            func(p []byte) []byte { p[5] = 2; return p },
		"first names prev": func(p []byte) []byte { p[30] = 1; return p },
		"later names none": func(p []byte) []byte { p[29] = 1; return p },
		"too much data":    func(p []byte) []byte { return append(p, make([]byte, MaxData+1)...) },
	} {
		if _, _, err := Parse(mut(good())); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if err := Params.ValidatePayload(mut(good())); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
	if _, _, err := Parse(good()); err != nil {
		t.Fatal(err)
	}
}

func TestFrames(t *testing.T) {
	var b bytes.Buffer
	if err := WritePreamble(&b); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&b, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if WriteFrame(&b, nil) == nil || WriteFrame(&b, make([]byte, MaxFrame+1)) == nil {
		t.Fatal("wrote a frame out of bounds")
	}
	r := bytes.NewReader(b.Bytes())
	if err := ReadPreamble(r); err != nil {
		t.Fatal(err)
	}
	if f, err := ReadFrame(r); err != nil || string(f) != "abc" {
		t.Fatalf("%q %v", f, err)
	}
	if _, err := ReadFrame(r); err != io.EOF {
		t.Fatalf("at the end: %v", err)
	}
	// A length out of bounds is refused before anything is allocated.
	for _, n := range [][]byte{{0, 0, 0, 0}, {0xff, 0xff, 0xff, 0xff}} {
		if _, err := ReadFrame(bytes.NewReader(n)); !errors.Is(err, ErrFrame) {
			t.Fatalf("length %x: %v", n, err)
		}
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 9, 1})); err != io.ErrUnexpectedEOF {
		t.Fatalf("a short frame: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 0})); err != io.ErrUnexpectedEOF {
		t.Fatalf("a short length: %v", err)
	}
	if err := ReadPreamble(bytes.NewReader([]byte("BEEFCAT\x02"))); !errors.Is(err, ErrFrame) {
		t.Fatalf("another framing version: %v", err)
	}
}
