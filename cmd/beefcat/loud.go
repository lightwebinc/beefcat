package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/lightwebinc/bcommon/termsafe"
	"github.com/lightwebinc/bcommon/verify"

	"github.com/lightwebinc/beefcat/recv"
)

// headline says in plain words what a refusal code means to the person
// reading the terminal.
func headline(c verify.Code) string {
	switch c {
	case verify.RefusedSig:
		return "FORGED OR ALTERED DATA"
	case verify.RefusedKeyDerive:
		return "FORGED SENDER IDENTITY"
	case verify.RefusedKey:
		return "THE SENDER CHANGED MID-STREAM"
	case verify.RefusedBump:
		return "FRAUDULENT POSTAGE: NOT PROVEN ON YOUR CHAIN"
	case verify.RefusedMineable:
		return "A MINEABLE TRANSACTION, NOT A DATA CARRIER"
	case verify.RefusedUnlocking:
		return "A CARRIER ANYONE COULD REWRITE"
	case verify.RefusedFork:
		return "ONE POSTAGE OUTPUT SPENT TWICE"
	case verify.RefusedSeq:
		return "STREAM REORDERED, SPLICED OR APPENDED TO"
	case recv.RefusedTruncated:
		return "STREAM CUT SHORT"
	case recv.RefusedRetracted:
		return "DATA RETRACTED BY ITS SENDER"
	case recv.RefusedStale:
		return "POSTAGE TOO OLD, OR NOT DEEP ENOUGH IN THE CHAIN"
	case recv.RefusedReplay:
		return "REPLAY: THIS DATA WAS ACCEPTED BEFORE"
	case recv.RefusedUntrusted:
		return "UNTRUSTED SENDER"
	}
	return "CORRUPT OR ALTERED: NOT A VALID CARRIER"
}

// isTerminal reports whether f is a terminal.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

const (
	red    = "\x1b[1;97;41m"
	yellow = "\x1b[1;30;43m"
	reset  = "\x1b[0m"
)

// banner prints lines between two bars, in color and with a bell on a
// terminal. It is never silenced: -q quiets success, not this. Every line
// is filtered first, since a reason can quote bytes the far side chose.
func (g *global) banner(color string, lines ...string) {
	tty := isTerminal(g.stderr) && os.Getenv("NO_COLOR") == ""
	bar := strings.Repeat("!", 76)
	var b strings.Builder
	b.WriteString("\n")
	put := func(s string) {
		if tty {
			b.WriteString(color + s + reset + "\n")
		} else {
			b.WriteString(s + "\n")
		}
	}
	put(bar)
	for _, l := range lines {
		for _, part := range wrap(termsafe.Text(l), 70) {
			put("!!  " + part)
		}
	}
	put(bar)
	if tty {
		b.WriteString("\a")
	}
	fmt.Fprint(g.stderr, b.String())
}

// wrap breaks s into lines of at most width characters, at spaces.
func wrap(s string, width int) []string {
	var out []string
	line := ""
	for _, w := range strings.Fields(s) {
		switch {
		case line == "":
			line = w
		case len(line)+1+len(w) > width:
			out = append(out, line)
			line = w
		default:
			line += " " + w
		}
	}
	if line != "" || len(out) == 0 {
		out = append(out, line)
	}
	return out
}
