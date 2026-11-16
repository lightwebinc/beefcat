package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/termsafe"
	"github.com/lightwebinc/bcommon/verify"

	"github.com/lightwebinc/beefcat/recv"
	"github.com/lightwebinc/beefcat/send"
	"github.com/lightwebinc/beefcat/wire"
)

const catHelp = `usage: beefcat [flags] HOST PORT        send standard input to HOST:PORT
       beefcat [flags] -o FILE          write the stream to FILE (- is standard output)
       beefcat [flags] -l [ADDR:]PORT   receive one stream and verify it

Sending cuts standard input into chunks and signs each into one carrier,
spending one funding output of the home's (beefcat fund). It asks nothing
of the chain. Over TCP it then waits for the far end's verdict and exits
with it.

Receiving writes a carrier's data to standard output only after that
carrier verified against your own header source. Pin the sender with -k:
without it, data from any sender whose carriers verify is written, and
beefcat says so loudly. A stream that fails part way leaves the data of
the carriers verified before it on standard output; with -save FILE,
nothing is kept unless the whole stream verified.`

// keyList is the -k flag: one or more sender keys.
type keyList [][33]byte

func (k *keyList) String() string { return fmt.Sprintf("%d key(s)", len(*k)) }

func (k *keyList) Set(s string) error {
	for _, part := range strings.Split(s, ",") {
		key, err := parseKey(part)
		if err != nil {
			return err
		}
		*k = append(*k, key)
	}
	return nil
}

// recvFlags are the flags of a reader.
type recvFlags struct {
	keys    keyList
	unspent bool
	save    string
}

func addRecvFlags(fs *flag.FlagSet, rf *recvFlags) {
	fs.Var(&rf.keys, "k", "receive: the sender's identity key (66 hex characters); repeat or separate with commas for several. Data from any other sender is refused")
	fs.BoolVar(&rf.unspent, "unspent", false, "receive: also ask the chain view whether each funding output is spent by another transaction, and refuse data its sender retracted")
	fs.StringVar(&rf.save, "save", "", "receive: write the data to FILE only if the whole stream verified, instead of standard output")
}

func cmdCat(ctx context.Context, g *global, args []string) (int, error) {
	fs := g.flagSet("beefcat", catHelp)
	var rf recvFlags
	addRecvFlags(fs, &rf)
	listen := fs.String("l", "", "receive: listen on [ADDR:]PORT for one stream")
	out := fs.String("o", "", "send: write the stream to FILE (- is standard output) instead of a connection")
	chunk := fs.String("chunk", "4M", "send: data per carrier, at most 4M (bytes, or with K or M)")
	lines := fs.Bool("lines", false, "send: one carrier per line of input, sent as it arrives")
	wait := fs.Duration("w", 2*time.Minute, "send: how long to wait for the far end's verdict; receive: how long a silent connection is kept")
	pos, err := g.parse(fs, args)
	if err != nil {
		return 0, err
	}
	if *listen != "" {
		if len(pos) > 0 || *out != "" {
			return 0, usage("-l takes no HOST PORT and no -o")
		}
		return g.listen(ctx, *listen, rf, *wait)
	}
	if len(rf.keys) > 0 || rf.unspent || rf.save != "" {
		return 0, usage("-k, -unspent and -save are for receiving (-l, or beefcat verify)")
	}
	size, err := parseSize(*chunk)
	if err != nil || size > wire.MaxData {
		return 0, usage("-chunk %s: a size of at most 4M", *chunk)
	}
	switch {
	case *out != "" && len(pos) == 0:
		return g.sendTo(ctx, "", *out, size, *lines, *wait)
	case *out == "" && len(pos) == 2:
		return g.sendTo(ctx, net.JoinHostPort(pos[0], pos[1]), "", size, *lines, *wait)
	}
	return 0, usage("beefcat HOST PORT, beefcat -o FILE, or beefcat -l PORT (beefcat help)")
}

// sendTo sends standard input to addr, or to the file out.
func (g *global) sendTo(ctx context.Context, addr, out string, chunk int, lines bool, wait time.Duration) (int, error) {
	h, err := g.openHome()
	if err != nil {
		return 0, err
	}
	defer h.close()
	// A file's size is known, so a send that cannot finish is refused
	// before a carrier is spent on it.
	if f, ok := g.stdin.(*os.File); ok && !lines {
		if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
			if need, have := send.Need(st.Size(), chunk), h.st.Remaining(); need > have {
				return 0, fmt.Errorf("this input takes %d carrier(s) and the home has postage for %d: run `beefcat fund -n %d`", need, have, need)
			}
		}
	}
	if h.st.NextTree() == nil {
		return 0, send.ErrNoPostage
	}
	var w io.Writer
	var conn *net.TCPConn
	switch {
	case out == "-":
		w = g.stdout
	case out != "":
		f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		w = f
	default:
		d := net.Dialer{Timeout: 30 * time.Second}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return 0, err
		}
		defer c.Close()
		conn = c.(*net.TCPConn)
		// A far end that stops reading does not hold the send forever.
		w = deadlineWriter{conn, wait}
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
	}
	// The far end answers with one line: after it has read to the end, or
	// as soon as it refuses a carrier. A refusal stops the send at once,
	// so no more postage is spent on a stream already refused.
	verdict := make(chan string, 1)
	sctx, stopSend := context.WithCancel(ctx)
	defer stopSend()
	if conn != nil {
		go func() {
			line, _ := bufio.NewReader(io.LimitReader(conn, 4096)).ReadString('\n')
			verdict <- strings.TrimSpace(line)
			stopSend()
		}()
	}
	bw := bufio.NewWriterSize(w, 1<<20)
	if lines {
		// A line is on the wire as soon as it is signed.
		w = flushWriter{bw}
	} else {
		w = bw
	}
	sg := h.e.Signer()
	s := &send.Sender{Wallet: sg, Originator: sg.Originator, State: h.st, Chunk: chunk, Lines: lines}
	copy(s.Identity[:], sg.IdentityKey().Compressed())
	start := time.Now()
	st, err := s.Stream(sctx, g.stdin, w)
	if ferr := bw.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		if conn != nil && ctx.Err() == nil {
			// The far end may have said why it stopped reading.
			select {
			case line := <-verdict:
				if line != "" {
					g.say("the send stopped after %d carrier(s), %d bytes, on the far end's verdict", st.Carriers, st.Bytes)
					return g.farEnd(line), nil
				}
			case <-time.After(5 * time.Second):
			}
		}
		if st.Carriers > 0 {
			g.banner(red, "BEEFCAT: THE SEND STOPPED PART WAY",
				fmt.Sprintf("%d carrier(s), %d bytes went out before: %v", st.Carriers, st.Bytes, err),
				"The stream has no signed last carrier, so the far end refuses it as cut short.")
			return exitError, nil
		}
		return 0, err
	}
	el := time.Since(start)
	g.say("sent %d carrier(s), %d bytes of data (%d on the wire) in %s; stream %x; postage left for %d carrier(s)",
		st.Carriers, st.Bytes, st.Wire, el.Round(time.Millisecond), st.Stream, h.st.Remaining())
	if conn == nil {
		return exitOK, nil
	}
	_ = conn.CloseWrite()
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	return g.farEnd(<-verdict), nil
}

// farEnd reports the far end's one-line verdict and returns the exit
// status it stands for.
func (g *global) farEnd(line string) int {
	word, rest, _ := strings.Cut(strings.TrimPrefix(line, "beefcat "), " ")
	switch {
	case line == "":
		g.say("the far end closed without a verdict: it may not be beefcat, or it stopped")
		return exitError
	case word == string(verify.Verified):
		g.say("the far end says: VERIFIED %s", termsafe.Text(rest))
		return exitOK
	case word == string(recv.RefusedUntrusted):
		g.banner(red, "THE FAR END REFUSED THIS DATA: IT DOES NOT TRUST THIS SENDER", line,
			"Give the far end this home's identity key (beefcat status) to pin with -k.")
		return exitUntrusted
	case strings.HasPrefix(word, "REFUSED-"):
		g.banner(red, "THE FAR END REFUSED THIS DATA: "+headline(verify.Code(word)), line)
		return exitRefused
	}
	g.banner(yellow, "THE FAR END REACHED NO VERDICT", line)
	return exitError
}

// deadlineWriter fails a write the far end does not take in time.
type deadlineWriter struct {
	c    net.Conn
	wait time.Duration
}

func (w deadlineWriter) Write(p []byte) (int, error) {
	if w.wait > 0 {
		_ = w.c.SetWriteDeadline(time.Now().Add(w.wait))
	}
	return w.c.Write(p)
}

type flushWriter struct{ *bufio.Writer }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.Writer.Write(p)
	if err == nil && f.Buffered() > 0 && len(p) > 4 {
		// After a frame's body, not after its four-byte length.
		err = f.Flush()
	}
	return n, err
}

// listen takes one connection, verifies its stream, and answers it with
// the verdict.
func (g *global) listen(ctx context.Context, addr string, rf recvFlags, idle time.Duration) (int, error) {
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	// Everything that can be refused is refused before the port opens.
	rd, err := g.reader(rf)
	if err != nil {
		return 0, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	g.say("listening on %s; proofs are checked against %s", ln.Addr(), g.cfg.Headers)
	conn, err := ln.Accept()
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, err
	}
	defer conn.Close()
	_ = ln.Close()
	stopConn := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopConn()
	g.say("connection from %s", conn.RemoteAddr())
	code, verdict := rd.run(ctx, idleReader{conn, idle})
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(conn, "beefcat "+verdict+"\n")
	// Closing with unread bytes resets the connection, and a reset can
	// destroy the verdict before the far end reads it. So the write side
	// is closed and what is still arriving is discarded, briefly.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.Copy(io.Discard, conn)
	return code, nil
}

// idleReader fails a read the far end leaves silent for too long.
type idleReader struct {
	c    net.Conn
	idle time.Duration
}

func (r idleReader) Read(p []byte) (int, error) {
	if r.idle > 0 {
		_ = r.c.SetReadDeadline(time.Now().Add(r.idle))
	}
	return r.c.Read(p)
}

const verifyHelp = `usage: beefcat verify [flags] [FILE]

Verify a stream written by "beefcat -o FILE", from FILE or standard input,
exactly as a listener verifies one, and write its data to standard output
(or, with -save, to a file only if the whole stream verified).`

func cmdVerify(ctx context.Context, g *global, args []string) (int, error) {
	fs := g.flagSet("verify", verifyHelp)
	var rf recvFlags
	addRecvFlags(fs, &rf)
	pos, err := g.parse(fs, args)
	if err != nil {
		return 0, err
	}
	in := g.stdin
	switch {
	case len(pos) > 1:
		return 0, usage("verify takes one FILE, or standard input")
	case len(pos) == 1 && pos[0] != "-":
		f, err := os.Open(pos[0])
		if err != nil {
			return 0, err
		}
		defer f.Close()
		in = f
	}
	rd, err := g.reader(rf)
	if err != nil {
		return 0, err
	}
	code, _ := rd.run(ctx, in)
	return code, nil
}

// reader is a configured reader: its header source, the keys it trusts,
// and where verified data goes.
type reader struct {
	g    *global
	opts recv.Options
	save string
}

func (g *global) reader(rf recvFlags) (*reader, error) {
	hc, err := g.headerClient()
	if err != nil {
		return nil, err
	}
	if rf.save != "" {
		// A file left by an earlier run would read as this run's output
		// to a script that checks only that it exists.
		if _, err := os.Lstat(rf.save); err == nil {
			return nil, usage("-save %s: the file exists; -save does not overwrite", rf.save)
		}
	}
	rd := &reader{g: g, opts: recv.Options{Headers: hc, Trusted: rf.keys}, save: rf.save}
	if rf.unspent {
		chain, err := g.chain(hc)
		if err != nil {
			return nil, err
		}
		rd.opts.Spends = chain
	}
	return rd, nil
}

// run verifies one stream from in and reports the outcome, loudly when it
// is anything but verified from a pinned sender. It returns the exit
// status and the one-line verdict a listener answers the far end with.
func (rd *reader) run(ctx context.Context, in io.Reader) (int, string) {
	g := rd.g
	var out io.Writer = g.stdout
	var tmp *os.File
	if rd.save != "" {
		var err error
		tmp, err = os.CreateTemp(filepath.Dir(rd.save), filepath.Base(rd.save)+".unverified.*")
		if err != nil {
			fmt.Fprintln(g.stderr, "beefcat:", err)
			return exitError, "ERROR the reader could not open its output"
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		out = tmp
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	start := time.Now()
	s, err := recv.Receive(ctx, bufio.NewReaderSize(in, 1<<20), bw, rd.opts)
	if ferr := bw.Flush(); err == nil && ferr != nil {
		err = fmt.Errorf("writing the output: %w", ferr)
	}
	if err == nil && tmp != nil {
		if err = tmp.Close(); err == nil {
			err = os.Rename(tmp.Name(), rd.save)
		}
		if err != nil {
			err = fmt.Errorf("writing %s: %w", rd.save, err)
		}
	}
	// What became of the data written so far.
	left := "Nothing was written."
	switch {
	case tmp != nil:
		left = fmt.Sprintf("%s was not written: nothing of this stream is kept.", rd.save)
	case s.Bytes > 0:
		left = fmt.Sprintf("%d bytes of the %d carrier(s) verified before it are already on standard output. THE OUTPUT IS INCOMPLETE: do not use it as the whole.", s.Bytes, s.Carriers)
	}
	var ref *recv.Refusal
	switch {
	case errors.As(err, &ref):
		lines := []string{
			"BEEFCAT REFUSED THIS DATA: " + headline(ref.Code),
			fmt.Sprintf("%s at carrier %d: %s", ref.Code, ref.Carrier, ref.Reason),
		}
		if s.HaveIdentity {
			lines = append(lines, fmt.Sprintf("The carriers verified before it were signed by %x.", s.Identity))
		}
		lines = append(lines, left)
		g.banner(red, lines...)
		fmt.Fprintf(g.stderr, "beefcat: %s carrier=%d\n", ref.Code, ref.Carrier)
		code := exitRefused
		if ref.Code == recv.RefusedUntrusted {
			code = exitUntrusted
		}
		return code, fmt.Sprintf("%s carrier=%d %s", ref.Code, ref.Carrier, ref.Reason)
	case err != nil:
		g.banner(yellow, "BEEFCAT REACHED NO VERDICT: THIS DATA IS NOT VERIFIED",
			err.Error(),
			"This is not a refusal: beefcat could not decide. "+left)
		fmt.Fprintf(g.stderr, "beefcat: %s\n", verify.Error)
		return exitError, fmt.Sprintf("%s the reader could not decide", verify.Error)
	}
	el := time.Since(start)
	who := fmt.Sprintf("%x", s.Identity)
	if !s.Pinned {
		g.banner(yellow, "BEEFCAT WARNING: THE SENDER IS NOT PINNED",
			"This data verified: it is intact, signed by the key below, and that key's postage is mined on your chain.",
			"But you did not say whose data you expected, and anyone with a funded key can send a stream that verifies. If you know the sender's key, pin it and run again:",
			"-k "+who)
	}
	if !g.quiet {
		pin := "pinned with -k"
		if !s.Pinned {
			pin = "NOT PINNED"
		}
		retract := "not checked (-unspent)"
		if s.Unspent {
			retract = "every funding output unspent"
		}
		fmt.Fprintf(g.stderr, "beefcat: %s %d carrier(s), %d bytes in %s\n  sender      %s (%s)\n  stream      %x\n  postage     funding transaction %s, mined at height %d\n  headers     %s\n  retraction  %s\n",
			verify.Verified, s.Carriers, s.Bytes, el.Round(time.Millisecond), who, pin, s.Stream, s.Tree, s.Height, g.cfg.Headers, retract)
	}
	return exitOK, fmt.Sprintf("%s carriers=%d bytes=%d", verify.Verified, s.Carriers, s.Bytes)
}
