package main

import (
	"os"
	"strings"
	"testing"
)

// A far end that closes without a verdict leaves the send unconfirmed: that
// is reported with a banner, which -q does not silence, and exits 1.
func TestFarEndCloseWithoutVerdictIsLoud(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g := &global{stderr: f, quiet: true}
	if code := g.farEnd(""); code != exitError {
		t.Fatalf("exit %d, want %d", code, exitError)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "THE FAR END CLOSED WITHOUT A VERDICT") {
		t.Fatalf("under -q the close without a verdict printed %q", b)
	}
}
