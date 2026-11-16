// Command beefcat is netcat for the BEEF data carrier: any bytes in, sent to
// an address and port as signed, unmineable carriers, and verified by SPV
// at the far end before a byte of them is written.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"

	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/nodeapi"

	"github.com/lightwebinc/beefcat/internal/home"
)

var version = "dev"

// buildVersion is the version a release build was stamped with, or else the
// module version the Go toolchain recorded, which is what "go install
// ...@v0.1.0" leaves.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return version
}

// Exit statuses. A refusal is never reported as an error, and an error is
// never reported as a refusal: an outage is not a forgery.
const (
	exitOK        = 0
	exitError     = 1 // could not decide, or could not run
	exitUsage     = 2
	exitRefused   = 3 // the data did not verify
	exitUntrusted = 4 // the data verified, from a sender who is not pinned
)

const usageText = `beefcat: netcat for the BEEF data carrier, with SPV

  beefcat [flags] HOST PORT          send standard input to HOST:PORT
  beefcat [flags] -o FILE            write the stream to FILE (- is standard output)
  beefcat [flags] -l [ADDR:]PORT     receive one stream, verify it, write it to standard output
  beefcat verify [flags] [FILE]      verify a stream from FILE or standard input

  beefcat init                       create the sender's home and identity
  beefcat fund                       mine postage: funding outputs for carriers
  beefcat status                     the home: identity, pool, postage left
  beefcat sweep                      spend funding outputs: retract what they carried
  beefcat withdraw                   pay coin out of the pool, to an address or an identity key
  beefcat version

Every chunk of data rides one carrier: a transaction that can never be
mined, signed by the sender, spending one mined funding output. The far end
checks each carrier against block headers it fetches itself, and writes a
carrier's data only after that carrier verified. Data that does not verify,
a sender who is not the one pinned, a stream cut short, and data its sender
retracted are refused loudly, with a nonzero exit status:

  0  verified        3  refused: the data did not verify
  1  no verdict      4  refused: the sender is not one you pinned
  2  usage

Run "beefcat <command> -h" for a command's flags.
`

// usageErr is a mistake in how the command was called.
type usageErr string

func (u usageErr) Error() string { return string(u) }

func usage(format string, args ...any) error { return usageErr(fmt.Sprintf(format, args...)) }

// global is one run: its streams and settings.
type global struct {
	stdin          io.Reader
	stdout, stderr *os.File
	home           string
	cfg            home.Config
	quiet          bool
	// set by the common flags, applied over the home's settings
	fNetwork, fHeaders, fChain, fSettle, fRPC string
}

func (g *global) say(format string, args ...any) {
	if !g.quiet {
		fmt.Fprintf(g.stderr, "beefcat: "+format+"\n", args...)
	}
}

// flagSet is a flag set with the flags every command takes.
func (g *global) flagSet(name, help string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(g.stderr)
	fs.Usage = func() {
		fmt.Fprint(g.stderr, help, "\n\nflags:\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&g.home, "home", defaultHome(), "the sender's home directory (BEEFCAT_HOME)")
	fs.StringVar(&g.fNetwork, "network", "", "main, test or regtest (default: the home's, else main)")
	fs.StringVar(&g.fHeaders, "headers", "", "header source every proof is checked against: woc:main, woc:test, chaintracks:URL, bhs:URL, arcade:URL, or a URL serving /v1/root and /v1/tip (default: the home's, else woc:<network>)")
	fs.StringVar(&g.fChain, "chain", "", "chain view for transactions, proofs and spends: woc:main, woc:test, asset:URL (default: the home's, else woc:<network>)")
	fs.BoolVar(&g.quiet, "q", false, "print nothing on success; refusals and errors are always printed")
	return fs
}

func defaultHome() string {
	if h := os.Getenv("BEEFCAT_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".beefcat")
	}
	return ".beefcat"
}

// parse parses args and resolves the settings: the home's, then the flags.
func (g *global) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errHelp
		}
		return nil, usageErr("")
	}
	if err := g.resolve(); err != nil {
		return nil, err
	}
	return fs.Args(), nil
}

// resolve settles the settings: the home's, then the flags, then what the
// network implies.
func (g *global) resolve() error {
	cfg, err := home.LoadConfig(g.home)
	if err != nil {
		return fmt.Errorf("the home %s: %w", g.home, err)
	}
	if g.fNetwork != "" && g.fNetwork != cfg.Network {
		// Another network's defaults, not the home's sources.
		cfg = home.Config{Network: g.fNetwork}
	}
	if g.fHeaders != "" {
		cfg.Headers = g.fHeaders
	}
	if g.fChain != "" {
		cfg.Chain = g.fChain
	}
	if g.fSettle != "" {
		cfg.Settle = g.fSettle
	}
	if g.fRPC != "" {
		cfg.RPC = g.fRPC
	}
	cfg.Defaults()
	switch cfg.Network {
	case "main", "test", "regtest":
	default:
		return usage("network %q: main, test or regtest", cfg.Network)
	}
	g.cfg = cfg
	return nil
}

var errHelp = errors.New("help")

// headerClient is the header source every proof is checked against.
func (g *global) headerClient() (*headers.Client, error) {
	if g.cfg.Headers == "" {
		return nil, usage("no header source: set -headers (network %s has no public one)", g.cfg.Network)
	}
	kind, _, implied, err := headers.Parse(g.cfg.Headers)
	if err != nil {
		return nil, usage("headers %q: %v", g.cfg.Headers, err)
	}
	if kind == headers.WhatsOnChain && implied != g.cfg.Network {
		return nil, usage("header source %s is the %s network but network is %s", g.cfg.Headers, implied, g.cfg.Network)
	}
	c, err := headers.NewSource(g.cfg.Headers)
	if err != nil {
		return nil, usage("headers %q: %v", g.cfg.Headers, err)
	}
	if c.Kind != headers.Native {
		c.Network = g.cfg.Network
	}
	return c, nil
}

// chain is the chain view, every proof it answers checked against hc.
func (g *global) chain(hc *headers.Client) (*nodeapi.Sources, error) {
	if g.cfg.Chain == "" {
		return nil, usage("no chain view: set -chain to asset:<node URL> (network %s has no public one)", g.cfg.Network)
	}
	c, err := nodeapi.ParseChain(g.cfg.Chain, nodeapi.ChainOptions{Headers: hc})
	if err != nil {
		return nil, usage("chain %q: %v", g.cfg.Chain, err)
	}
	return c, nil
}

// parseKey reads a compressed public key in hex.
func parseKey(s string) ([33]byte, error) {
	var k [33]byte
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 33 {
		return k, fmt.Errorf("%q is not a compressed public key (66 hex characters)", s)
	}
	if _, err := guard.ParsePubKey(b); err != nil {
		return k, fmt.Errorf("%q is not a public key: %v", s, err)
	}
	copy(k[:], b)
	return k, nil
}

// parseSize reads a byte count with an optional K or M (binary) suffix.
func parseSize(s string) (int, error) {
	mul := 1
	switch {
	case strings.HasSuffix(s, "K"), strings.HasSuffix(s, "k"):
		mul, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		mul, s = 1<<20, s[:len(s)-1]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > (1<<31)/mul {
		return 0, fmt.Errorf("%q is not a size (bytes, or with K or M)", s)
	}
	return n * mul, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The first signal asks the command to stop; a second one kills it.
	go func() {
		<-ctx.Done()
		stop()
	}()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr *os.File) int {
	g := &global{stdin: stdin, stdout: stdout, stderr: stderr}
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	var code int
	var err error
	switch args[0] {
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, "beefcat", buildVersion())
		return exitOK
	case "init":
		err = cmdInit(ctx, g, args[1:])
	case "fund":
		err = cmdFund(ctx, g, args[1:])
	case "status":
		err = cmdStatus(ctx, g, args[1:])
	case "sweep":
		err = cmdSweep(ctx, g, args[1:])
	case "withdraw":
		err = cmdWithdraw(ctx, g, args[1:])
	case "verify":
		code, err = cmdVerify(ctx, g, args[1:])
	default:
		code, err = cmdCat(ctx, g, args)
	}
	var u usageErr
	switch {
	case err == nil:
		return code
	case errors.Is(err, errHelp):
		return exitOK
	case errors.As(err, &u):
		if u != "" {
			fmt.Fprintln(stderr, "beefcat:", string(u))
		}
		return exitUsage
	}
	fmt.Fprintln(stderr, "beefcat:", err)
	if code != 0 {
		return code
	}
	return exitError
}
