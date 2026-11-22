package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/unicast"

	"github.com/lightwebinc/beefcat/internal/home"
	"github.com/lightwebinc/beefcat/wire"
)

// poll paces proof waits; tests shorten it.
var poll = 5 * time.Second

// treeSats is each funding output's value. A carrier carries it forward
// and is never mined, so one satoshi is all an output needs.
const treeSats = 1

// opened is an open, locked home.
type opened struct {
	e     *bwallet.Embedded
	st    *home.State
	close func()
}

// openHome locks the home, then opens its wallet and state. The lock comes
// first: the pool is read when the wallet opens and written back whole.
func (g *global) openHome() (*opened, error) {
	if _, err := os.Stat(filepath.Join(g.home, "identity.json")); err != nil {
		return nil, fmt.Errorf("no sender's home at %s: run `beefcat init`", g.home)
	}
	unlock, err := home.Lock(g.home)
	if err != nil {
		return nil, err
	}
	e, err := g.wallet()
	if err != nil {
		unlock()
		return nil, err
	}
	st, err := home.LoadState(g.home, e.Signer().IdentityHex())
	if err != nil {
		unlock()
		return nil, err
	}
	return &opened{e: e, st: st, close: unlock}, nil
}

func (g *global) wallet() (*bwallet.Embedded, error) {
	e, err := bwallet.Open(g.home, wire.Profile)
	if err != nil {
		return nil, fmt.Errorf("open the home %s: %w (run `beefcat init`)", g.home, err)
	}
	e.Originator = "beefcat"
	e.Mainnet = g.cfg.Network == "main"
	return e, nil
}

// treeCost is roughly what a funding tree of n outputs costs: each
// output's satoshi and the miner fee on the tree's size.
func treeCost(n int) uint64 {
	size := 200 + 58*n
	fee, err := mint.DefaultFees.For(size)
	if err != nil {
		fee = uint64(size)
	}
	return uint64(n)*treeSats + fee //nolint:gosec // a small count
}

const initHelp = `usage: beefcat init [-network main|test|regtest] [-devchain URL]

Create the sender's home (mode 0700): an identity key and an empty coin
pool, and record the network and its sources. Print the identity key,
which is what a receiver pins with -k, and the fund address. A home that
exists is left alone and printed.

Only a sender needs a home. A receiver needs none.

-devchain URL records a local stand-in chain (cmd/devchain) as the header
source, chain view, settlement leg and node, and sets network regtest.`

func cmdInit(_ context.Context, g *global, args []string) error {
	fs := g.flagSet("init", initHelp)
	fs.StringVar(&g.fSettle, "settle", "", "settlement leg: arcade:main, arcade:test, arcade:URL, arc:URL, rpc:URL")
	fs.StringVar(&g.fRPC, "rpc", "", "a node's JSON-RPC, for coinbase on a regtest chain you run")
	dev := fs.String("devchain", "", "a local stand-in chain's URL (cmd/devchain): sets network regtest and every source to it")
	pos, err := g.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usage("init takes no arguments")
	}
	if *dev != "" {
		u := strings.TrimRight(*dev, "/")
		g.fNetwork, g.fHeaders, g.fChain, g.fSettle, g.fRPC = "regtest", u, "asset:"+u, "rpc:"+u+"/rpc", u+"/rpc"
		if err := g.resolve(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(g.home, 0o700); err != nil {
		return err
	}
	unlock, err := home.Lock(g.home)
	if err != nil {
		return err
	}
	defer unlock()
	e, err := bwallet.Create(g.home, wire.Profile)
	switch {
	case errors.Is(err, bwallet.ErrIdentityExists):
		// The home's recorded settings stand; flags given now are saved
		// over them.
		if e, err = g.wallet(); err != nil {
			return err
		}
		g.say("the home %s exists; its identity is left as it is", g.home)
	case err != nil:
		return err
	default:
		fmt.Fprintln(g.stdout, "created", g.home)
	}
	if err := os.Chmod(g.home, 0o700); err != nil {
		return err
	}
	if err := home.SaveConfig(g.home, g.cfg); err != nil {
		return err
	}
	e.Mainnet = g.cfg.Network == "main"
	return g.printIdentity(e)
}

func (g *global) printIdentity(e *bwallet.Embedded) error {
	addr, err := e.FundAddress(e.Mainnet)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "identity     %s\nfund address %s (%s)\n", e.Signer().IdentityHex(), addr, g.cfg.Network)
	return nil
}

const statusHelp = `usage: beefcat status

Print the home: its identity key (what a receiver pins with -k), fund
address, coin pool and the postage left. Reads only. With -age it asks
the header source for its tip and prints how old each tree's postage is.`

func cmdStatus(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("status", statusHelp)
	age := fs.Bool("age", false, "ask the header source for its tip, and print each tree's age in blocks")
	if pos, err := g.parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("status takes no arguments")
	}
	e, err := g.wallet()
	if err != nil {
		return err
	}
	st, err := home.LoadState(g.home, e.Signer().IdentityHex())
	if err != nil {
		return err
	}
	tip, haveTip := uint32(0), false
	if *age {
		hc, err := g.headerClient()
		if err != nil {
			return err
		}
		if tip, err = hc.CurrentHeight(ctx); err != nil {
			return fmt.Errorf("-age: the header source %s did not give its tip: %w", g.cfg.Headers, err)
		}
		haveTip = true
	}
	out := g.stdout
	fmt.Fprintf(out, "home         %s\nnetwork      %s\n", g.home, g.cfg.Network)
	if err := g.printIdentity(e); err != nil {
		return err
	}
	fmt.Fprintf(out, "headers      %s\nchain        %s\nsettle       %s\n", orNone(g.cfg.Headers), orNone(g.cfg.Chain), orNone(g.cfg.Settle))
	fmt.Fprintf(out, "pool         %d output(s), %d sat\n", e.Pool.Count(), e.Pool.Balance())
	fmt.Fprintf(out, "postage      %d carrier(s) left (up to %d bytes of data)\n", st.Remaining(), st.Remaining()*wire.MaxData)
	for _, t := range st.Trees {
		state := fmt.Sprintf("mined at height %d", t.Height)
		if haveTip && t.BumpHex != "" {
			old := uint32(0)
			if tip > t.Height {
				old = tip - t.Height
			}
			state += fmt.Sprintf(", %d block(s) old at tip %d", old, tip)
		}
		if t.BumpHex == "" {
			state = "not mined yet"
		}
		if sw := sweepOf(st, t.Txid); sw != nil {
			state = "swept by " + sw.Txid
			if sw.Height == 0 {
				state += " (not mined yet)"
			}
		}
		fmt.Fprintf(out, "tree         %s %d of %d left, %s\n", t.Txid, t.Remaining(), t.Count, state)
	}
	for _, p := range st.Pending {
		fmt.Fprintf(out, "tree         %s signed and not recorded as mined; `beefcat fund` asks what became of it\n", p.Tree.Txid)
	}
	fmt.Fprintf(out, "sent         %d carrier(s), %d bytes of data\n", st.SentCarriers, st.SentBytes)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "not configured"
	}
	return s
}

func sweepOf(st *home.State, tree string) *home.Sweep {
	for i := range st.Sweeps {
		if st.Sweeps[i].Tree == tree {
			return &st.Sweeps[i]
		}
	}
	return nil
}

// engine is what fund and sweep mine through.
type engine struct {
	g     *global
	h     *opened
	hc    *headers.Client
	chain *nodeapi.Sources
	payer *producer.Payer
	trees *producer.Trees
}

// treeState gives producer.Trees the home's record. There is no current
// tree: fund mints a tree whenever it is asked for one, and a send takes
// outputs from every mined tree in turn.
type treeState struct{ e *engine }

func (treeState) Current() *funding.Tree { return nil }

func (t treeState) Adopt(tree funding.Tree) error {
	st := t.e.h.st
	if st.Tree(tree.Txid) == nil {
		st.Trees = append(st.Trees, tree)
	}
	st.DropPending(tree.Txid)
	return st.Save()
}

func (g *global) engine(ctx context.Context, h *opened, wait time.Duration) (*engine, error) {
	hc, err := g.headerClient()
	if err != nil {
		return nil, err
	}
	chain, err := g.chain(hc)
	if err != nil {
		return nil, err
	}
	if g.cfg.Settle == "" {
		return nil, usage("no settlement leg: set -settle (rpc:URL, arcade:URL or arc:URL); network %s has no public one", g.cfg.Network)
	}
	settler, _, err := publish.ParseSettler(g.cfg.Settle, publish.SettleOptions{RPCID: "beefcat", Spends: chain, Note: g.say})
	if err != nil {
		return nil, usage("settle %q: %v", g.cfg.Settle, err)
	}
	tip, err := hc.CurrentHeight(ctx)
	if err != nil {
		return nil, fmt.Errorf("the header source %s: %w", g.cfg.Headers, err)
	}
	e := &engine{g: g, h: h, hc: hc, chain: chain}
	sg := h.e.Signer()
	kept := &producer.Kept{Load: func(txid string) (*transaction.Transaction, error) {
		if t := h.st.Tree(txid); t != nil {
			return funding.Rebuild(t.RawHex, t.BumpHex, t.BeefHex)
		}
		return nil, fmt.Errorf("transaction %s is not a funding tree this home keeps", txid)
	}}
	e.payer = &producer.Payer{
		Pool: h.e.Pool, Tip: tip, Keys: map[string]*bwallet.Signer{sg.IdentityHex(): sg},
		Kept: kept, Settler: settler, Chain: chain, Fees: mint.DefaultFees,
		Timeout: wait, Poll: poll, Note: func(format string, args ...any) {
			if !strings.HasPrefix(format, "funding tree published") {
				g.say(format, args...)
			}
		},
	}
	e.trees = &producer.Trees{
		Payer: e.payer, State: treeState{e}, Identity: sg.IdentityHex(), Sats: treeSats, Funder: "pool",
		Lock: func(ctx context.Context) (*script.Script, error) {
			return carrier.FundingLock(ctx, sg, sg.Originator, wire.Params)
		},
		Change: sg.FundScript,
		// A funding tree is published nowhere: every carrier carries its own.
		Facade: unicast.Kept(), Topic: "tm_beefcat_kept",
		Prepare: func(tree funding.Tree, coin bwallet.Output) error {
			h.st.Pending = append(h.st.Pending, home.Pending{Tree: tree, Coin: coin})
			if err := h.st.Save(); err != nil {
				h.st.DropPending(tree.Txid)
				return err
			}
			return nil
		},
	}
	return e, nil
}

// recover settles every tree a run signed and did not record as mined.
func (e *engine) recover(ctx context.Context) error {
	st := e.h.st
	for _, p := range append([]home.Pending(nil), st.Pending...) {
		id := p.Tree.Txid
		if t := st.Tree(id); t != nil {
			st.DropPending(id)
			if err := st.Save(); err != nil {
				return err
			}
			continue
		}
		r, err := e.trees.Recover(ctx, p.Tree, p.Coin)
		if errors.Is(err, producer.ErrPublish) {
			err = nil
		}
		if err != nil {
			e.g.say("funding tree %s was broadcast by an earlier run and is not mined yet, or the chain view could not answer (%v); run `beefcat fund` again later", id, err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		switch r.Outcome {
		case producer.TreeAdopted, producer.TreeHeld:
			if st.Tree(id) == nil {
				if err := (treeState{e}).Adopt(r.Tree); err != nil {
					return err
				}
			}
			e.g.say("funding tree %s from an earlier run is mined: %d carrier(s) of postage", id, r.Tree.Count)
		case producer.CoinSpent:
			st.DropPending(id)
			e.g.say("funding tree %s from an earlier run never mined (its coin was spent by %s); forgotten", id, r.By)
		case producer.CoinReturned:
			// A tree handed over an instant before a run stopped can still
			// land, so the record is kept for one more command.
			for i := range st.Pending {
				if st.Pending[i].Tree.Txid == id {
					if st.Pending[i].Asked++; st.Pending[i].Asked >= 2 {
						st.DropPending(id)
					}
					break
				}
			}
			e.g.say("funding tree %s from an earlier run did not reach the chain; its coin is back in the pool", id)
		}
		if err := st.Save(); err != nil {
			return err
		}
	}
	return nil
}

// collect asks the chain view what became of each transaction whose change
// the pool holds back: a payment that had not mined when withdraw stopped
// waiting for it. Change from one that has mined becomes spendable. Change
// from one the network refused, an input of it spent by another
// transaction, is taken out of the pool with a banner, since that
// transaction never mines. One the chain view cannot answer for stays held.
func (e *engine) collect(ctx context.Context) error {
	pool := e.h.e.Pool
	held := map[string][]bwallet.Output{}
	for _, o := range pool.Outputs() {
		if o.Unproven {
			held[o.TxID] = append(held[o.TxID], o)
		}
	}
	proofs := producer.Proofs{Source: e.chain, Spends: e.chain, Tx: func(txid string) (*transaction.Transaction, error) {
		raw, err := hex.DecodeString(held[txid][0].Raw)
		if err != nil {
			return nil, err
		}
		return guard.ParseTransaction(raw, guard.DefaultBound)
	}}
	for _, txid := range pool.UnprovenTxids() {
		outs := held[txid]
		if len(outs) == 0 {
			continue
		}
		mp, height, err := proofs.Of(ctx, txid)
		switch {
		case err == nil:
			n, err := pool.Prove(txid, funding.BumpHex(mp), height)
			if err != nil {
				return err
			}
			e.g.say("change from %s: mined at height %d; %d coin(s) spendable", txid, height, n)
		case errors.Is(err, producer.ErrRefused):
			var sats uint64
			for _, o := range outs {
				if _, err := pool.Remove(o); err != nil {
					return err
				}
				sats += o.Satoshis
			}
			if err := e.g.refusedPayment(txid, err, fmt.Sprintf("Its change, %d sat, was held in the pool until it mined, and is taken out.", sats)); err != nil {
				return err
			}
		case !errors.Is(err, nodeapi.ErrNotMined):
			e.g.say("change from %s is held: the chain view could not say whether it mined (%v)", txid, err)
		}
	}
	return nil
}

const fundHelp = `usage: beefcat fund [-n N] [-txid TXID | -beef FILE|-]
       beefcat fund [-n N] -coinbase        (regtest only)

Make sure the home has postage for N carriers (default 64, 256 MiB of
data at the default chunk): mint one funding tree with the outputs that
are missing, broadcast it, and wait for the block that mines it. One
carrier spends one output, and a tree holds at most 1023. A home that
already has N is left alone, so a fund that timed out waiting for a block
is simply run again.

The tree is paid for from the home's pool. Put coin in the pool by sending
a payment from your own wallet to the fund address (beefcat status), then:

  -txid TXID   import that payment once it is mined, read from the chain
               view and checked against the header source
  -beef FILE   import that payment as the BEEF your wallet hands over
               (binary or hex; - is standard input), with no lookup

A tree of N outputs costs N satoshis and the miner fee on about
190 + 58*N bytes: 64 outputs, about 460 satoshis.

-coinbase mines coinbase to the fund address through the node's
generatetoaddress. Coinbase: only on a regtest chain you run, for
development and tests.`

func cmdFund(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("fund", fundHelp)
	fs.StringVar(&g.fSettle, "settle", "", "settlement leg (default: the home's, else arcade:<network>)")
	fs.StringVar(&g.fRPC, "rpc", "", "a node's JSON-RPC, for -coinbase")
	n := fs.Int("n", 64, "how many carriers of postage the home should have")
	txid := fs.String("txid", "", "import this mined payment to the fund address first")
	beef := fs.String("beef", "", "import this payment's BEEF first: a file, or - for standard input")
	coinbase := fs.Bool("coinbase", false, "coinbase: only on a regtest chain you run (development and tests); mine 101 blocks to the fund address first")
	wait := fs.Duration("wait", 30*time.Minute, "how long to wait for the block that mines the tree")
	var maxAge blockSpan
	fs.Var(&maxAge, "max-age", "count only postage no older than this toward N, and mint what is missing. "+maxAgeUsage)
	pos, err := g.parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) > 0:
		return usage("fund takes no arguments")
	case *n < 0:
		return usage("-n must not be negative")
	case (*txid != "" && *beef != "") || (*coinbase && (*txid != "" || *beef != "")):
		return usage("-txid, -beef and -coinbase are exclusive")
	case *coinbase && g.cfg.Network != "regtest":
		return usage("-coinbase mines coinbase (coinbase: only on a regtest chain you run, for development and tests); on network %s, import a payment with -txid or -beef", g.cfg.Network)
	}
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	e, err := g.engine(ctx, h, *wait)
	if err != nil {
		return err
	}
	if err := e.collect(ctx); err != nil {
		return err
	}
	if err := e.recover(ctx); err != nil {
		return err
	}
	sg := h.e.Signer()
	switch {
	case *txid != "" || *beef != "":
		fund, err := sg.FundScript()
		if err != nil {
			return err
		}
		var im *bwallet.Import
		if *txid != "" {
			im, err = bwallet.ImportTxid(ctx, *txid, fund, e.chain, e.hc)
		} else {
			var raw []byte
			if raw, err = readBEEF(g.stdin, *beef); err != nil {
				return err
			}
			im, err = bwallet.ImportBEEF(ctx, raw, fund, e.hc, bwallet.ImportOptions{RefuseUnmined: true})
		}
		if err != nil {
			return g.importError(h.e, err)
		}
		added, err := h.e.Pool.Add(im.Outputs...)
		if err != nil {
			return err
		}
		g.say("imported %d output(s), %d sat, mined at height %d; pool %d output(s), %d sat", added, im.Sats, im.Height, h.e.Pool.Count(), h.e.Pool.Balance())
	case *coinbase:
		if g.cfg.RPC == "" || !strings.HasPrefix(g.cfg.Chain, "asset:") {
			return usage("-coinbase needs a node: -rpc URL and -chain asset:URL")
		}
		rpc := &nodeapi.RPC{URL: g.cfg.RPC, ID: "beefcat"}
		asset := &nodeapi.Asset{Base: strings.TrimRight(strings.TrimPrefix(strings.Split(g.cfg.Chain, ",")[0], "asset:"), "/")}
		added, _, err := bwallet.FundFromCoinbase(ctx, sg, h.e.Pool, rpc, asset, 101, 0)
		if err != nil {
			return err
		}
		if tip, err := e.hc.CurrentHeight(ctx); err == nil {
			e.payer.Tip = tip
		}
		g.say("mined coinbase: %d output(s) added; pool %d output(s), %d sat", added, h.e.Pool.Count(), h.e.Pool.Balance())
	}
	// With -max-age, postage too old for a far end that limits its age
	// does not count: what is missing is minted fresh, and the old stays.
	// The cutoff is taken again after every tree, since each costs a block.
	minHeight, ageFlag := uint32(0), ""
	minted, nextMin := 0, uint32(0)
	mostTrees := (*n+mint.MaxFundingOutputs-1)/mint.MaxFundingOutputs + 1
	if uint64(maxAge) < 1<<20 {
		mostTrees += int(maxAge) + 1
	}
	cutoff := func() error {
		if maxAge == 0 {
			return nil
		}
		tip, err := e.hc.CurrentHeight(ctx)
		if err != nil {
			return fmt.Errorf("-max-age: the header source %s did not give its tip: %w", g.cfg.Headers, err)
		}
		minHeight, nextMin = 0, 0
		if tip > uint32(maxAge) {
			minHeight = tip - uint32(maxAge)
		}
		if tip+1 > uint32(maxAge) {
			nextMin = tip + 1 - uint32(maxAge)
		}
		return nil
	}
	if maxAge > 0 {
		ageFlag = fmt.Sprintf(" -max-age %d", maxAge)
		// A tree is mined in a block of its own, so a window of maxAge
		// blocks holds at most maxAge+1 trees. A target beyond that is
		// never reached, and every tree minted toward it would be paid
		// for and aged out.
		if most := (uint64(maxAge) + 1) * mint.MaxFundingOutputs; uint64(*n) > most { //nolint:gosec // checked non-negative
			return usage("-n %d -max-age %d: postage no more than %d block(s) old is at most %d tree(s) of %d output(s), %d carrier(s); nothing was minted", *n, maxAge, maxAge, uint64(maxAge)+1, mint.MaxFundingOutputs, most)
		}
	}
	for {
		if err := cutoff(); err != nil {
			return err
		}
		young := h.st.RemainingFrom(minHeight)
		if young >= uint64(*n) { //nolint:gosec // checked non-negative
			break
		}
		// Postage the home held can age out while a tree is mined, and is
		// then minted again: at a block a tree, that is at most the trees
		// the window holds, on top of the trees the target takes. Past
		// that, blocks are passing faster than trees are mined, and
		// postage ages out as fast as it is paid for.
		if maxAge > 0 && minted >= mostTrees {
			return fmt.Errorf("minted %d tree(s) and the home has postage for %d carrier(s) no more than %d block(s) old, not %d: blocks are passing faster than trees are mined. Nothing more was minted; a longer -max-age, or a smaller -n, can be reached", minted, young, maxAge, *n)
		}
		// The tree is mined a block from now, when the window has moved
		// on by one: it is sized against the postage still inside then.
		count := int(uint64(*n) - h.st.RemainingFrom(nextMin)) //nolint:gosec // checked non-negative
		if count > mint.MaxFundingOutputs {
			count = mint.MaxFundingOutputs
		}
		e.trees.Count = count
		before := len(h.st.Trees)
		if _, _, err := e.trees.Spend(ctx, uint32(count)); err != nil { //nolint:gosec // at most 1023
			var nc *producer.NoCoinError
			if errors.As(err, &nc) {
				addr, _ := h.e.FundAddress(h.e.Mainnet)
				if nc.Held > 0 {
					return fmt.Errorf("the pool's coin is change from %d transaction(s) not mined yet: run `beefcat fund%s` again after the next block", nc.Held, ageFlag)
				}
				return fmt.Errorf("the pool cannot pay for a tree of %d output(s), about %d sat: send a payment to the fund address %s from your own wallet, then `beefcat fund -n %d%s -txid <its txid>`", count, treeCost(count), addr, *n, ageFlag)
			}
			if len(h.st.Pending) > 0 {
				return fmt.Errorf("%w\nthe tree may still be mined: run `beefcat fund -n %d%s` again later, which asks what became of it and mints nothing twice", err, *n, ageFlag)
			}
			return err
		}
		minted++
		if len(h.st.Trees) == before || h.st.Trees[len(h.st.Trees)-1].BumpHex == "" {
			return fmt.Errorf("the funding tree was broadcast and is not recorded as mined; run `beefcat fund%s` again", ageFlag)
		}
	}
	if maxAge > 0 {
		fmt.Fprintf(g.stdout, "postage for %d carrier(s) no more than %d block(s) old, %d in all; pool %d output(s), %d sat\n",
			h.st.RemainingFrom(minHeight), maxAge, h.st.Remaining(), h.e.Pool.Count(), h.e.Pool.Balance())
		return nil
	}
	fmt.Fprintf(g.stdout, "postage for %d carrier(s), up to %d bytes of data; pool %d output(s), %d sat\n",
		h.st.Remaining(), h.st.Remaining()*wire.MaxData, h.e.Pool.Count(), h.e.Pool.Balance())
	return nil
}

// readBEEF reads a BEEF, binary or hex, from a file or standard input.
func readBEEF(stdin io.Reader, path string) ([]byte, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, usage("-beef: %v", err)
		}
		defer f.Close()
		r = f
	}
	const bound = 2*guard.DefaultBound + 2
	b, err := io.ReadAll(io.LimitReader(r, bound+1))
	if err != nil {
		return nil, fmt.Errorf("-beef: %w", err)
	}
	if len(b) > bound {
		return nil, usage("-beef: more than %d bytes; a funding payment is far smaller", bound)
	}
	if t := bytes.TrimSpace(b); len(t) > 0 && len(t)%2 == 0 {
		if raw, err := hex.DecodeString(string(t)); err == nil {
			b = raw
		}
	}
	return b, nil
}

// importError words an import's refusal.
func (g *global) importError(e *bwallet.Embedded, err error) error {
	addr, _ := e.FundAddress(e.Mainnet)
	switch {
	case errors.Is(err, bwallet.ErrPaysNothing):
		return fmt.Errorf("the payment pays nothing to the fund address %s", addr)
	case errors.Is(err, bwallet.ErrUnmined):
		return errors.New("the payment has not mined yet: import it once it has a block")
	case errors.Is(err, nodeapi.ErrProofRefused):
		return fmt.Errorf("the payment's proof does not hold against the header source %s", g.cfg.Headers)
	}
	return fmt.Errorf("the payment: %w", err)
}

const sweepHelp = `usage: beefcat sweep [-tree TXID] -yes

Spend every funding output of the home's trees (or of one, with -tree) in
one mined transaction per tree, and wait for its block. This is the
retraction: a carrier is the one spender its funding output is meant to
have, so once the sweep is mined, a receiver that checks spends
(-unspent) refuses everything those outputs carried, and the outputs not
yet used are gone with them. It costs a miner fee and returns the outputs'
few satoshis; it is not a way to recover coin.

A sweep not mined within -wait is waited for again by the next sweep -yes.
A sweep the network refuses, because a coin it spends is spent by another
transaction, never mines and retracts nothing: a banner says so, and the
next sweep -yes sweeps the tree with a new transaction. When that coin is
one of the tree's outputs, which only this home's key can spend, another
copy of the home spent it: that spend retracts what it spends once mined,
and no sweep of the whole tree can mine beside it.

Nothing is spent without -yes.`

func cmdSweep(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("sweep", sweepHelp)
	fs.StringVar(&g.fSettle, "settle", "", "settlement leg (default: the home's, else arcade:<network>)")
	only := fs.String("tree", "", "sweep only this funding tree")
	yes := fs.Bool("yes", false, "actually sweep; without it nothing is spent")
	wait := fs.Duration("wait", 30*time.Minute, "how long to wait for the block that mines a sweep")
	pos, err := g.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usage("sweep takes no arguments")
	}
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	st := h.st
	if *only != "" && st.Tree(*only) == nil {
		return usage("-tree %s is not a funding tree of this home", *only)
	}
	var todo []string
	for _, t := range st.Trees {
		if (*only == "" || *only == t.Txid) && t.BumpHex != "" {
			if sw := sweepOf(st, t.Txid); sw == nil || sw.Height == 0 {
				todo = append(todo, t.Txid)
			}
		}
	}
	if len(todo) == 0 {
		fmt.Fprintln(g.stdout, "nothing to sweep")
		return nil
	}
	if !*yes {
		for _, id := range todo {
			t := st.Tree(id)
			fmt.Fprintf(g.stdout, "would sweep tree %s: %d output(s), %d of them carrying data already sent\n", id, t.Count, t.Next)
		}
		fmt.Fprintln(g.stdout, "nothing was spent; run again with -yes")
		return nil
	}
	e, err := g.engine(ctx, h, *wait)
	if err != nil {
		return err
	}
	if err := e.collect(ctx); err != nil {
		return err
	}
	sg := h.e.Signer()
	for _, id := range todo {
		t := st.Tree(id)
		var tx *transaction.Transaction
		if sw := sweepOf(st, id); sw != nil {
			// A sweep an earlier run broadcast: only its block is waited for.
			raw, err := hex.DecodeString(sw.RawHex)
			if err != nil {
				return err
			}
			if tx, err = guard.ParseTransaction(raw, guard.DefaultBound); err != nil {
				return err
			}
			g.say("sweep %s of tree %s was broadcast by an earlier run; waiting for its block", sw.Txid, id)
			mp, height, err := e.payer.Await(ctx, "sweep", tx)
			if errors.Is(err, producer.ErrRefused) {
				// It never mines, so its record goes and the next run sweeps
				// the tree anew. The tree stays closed to sends: it was
				// being retracted, and which of its outputs were used before
				// is not recorded.
				st.Sweeps = slices.DeleteFunc(st.Sweeps, func(s home.Sweep) bool { return s.Tree == id })
				if serr := st.Save(); serr != nil {
					return serr
				}
				g.refusedSweep(id, t.Count, tx, err, true)
				return fmt.Errorf("sweep of tree %s: the network refused it", id)
			}
			if err != nil {
				return fmt.Errorf("%w (run `beefcat sweep -yes` again later)", err)
			}
			e.payer.Change(tx, height, mp)
			sw.Height = height
			if err := st.Save(); err != nil {
				return err
			}
			fmt.Fprintf(g.stdout, "swept tree %s in %s, mined at height %d\n", id, sw.Txid, height)
			continue
		}
		tree, err := e.payer.Kept.Tx(id)
		if err != nil {
			return err
		}
		vouts := make([]uint32, t.Count)
		for i := range vouts {
			vouts[i] = uint32(i) //nolint:gosec // at most 1023
		}
		fee, err := e.payer.TakeAtLeast(ctx, mint.DefaultFees.Floor+uint64(len(vouts))*15)
		if err != nil {
			return fmt.Errorf("sweep fee: %w (the sweep's miner fee comes from the pool: fund it with `beefcat fund -n 0 -txid <payment>`)", err)
		}
		change, err := sg.FundScript()
		if err != nil {
			e.payer.GiveBack()
			return err
		}
		tx, err = carrier.SweepAt(ctx, sg, sg.Originator, wire.Params, tree, vouts, fee.Tx, fee.Vout, fee.Unlocker, change, mint.DefaultFees)
		if err != nil {
			e.payer.GiveBack()
			return fmt.Errorf("sweep of tree %s: %w", id, err)
		}
		// Recorded before it is broadcast, with the tree closed to sends:
		// an output swept must never carry a new carrier.
		used := t.Next
		t.Next = t.Count
		st.Sweeps = append(st.Sweeps, home.Sweep{Tree: id, Txid: tx.TxID().String(), RawHex: tx.Hex()})
		if err := st.Save(); err != nil {
			e.payer.GiveBack()
			return err
		}
		mp, height, err := e.payer.SettleAndWait(ctx, "sweep", tx)
		if err != nil {
			refused := errors.Is(err, producer.ErrRefused)
			if refused || !strings.Contains(err.Error(), "waiting for a proof") {
				// It never reached the chain, or never mines: the record is
				// undone.
				st.Sweeps = st.Sweeps[:len(st.Sweeps)-1]
				t.Next = used
				e.payer.GiveBack()
				if serr := st.Save(); serr != nil {
					return serr
				}
				if refused {
					g.refusedSweep(id, t.Count, tx, err, false)
					return fmt.Errorf("sweep of tree %s: the network refused it", id)
				}
				return fmt.Errorf("sweep of tree %s: %w", id, err)
			}
			return fmt.Errorf("%w (the sweep is broadcast; run `beefcat sweep -yes` again later)", err)
		}
		e.payer.Change(tx, height, mp)
		st.Sweeps[len(st.Sweeps)-1].Height = height
		if err := st.Save(); err != nil {
			return err
		}
		fmt.Fprintf(g.stdout, "swept tree %s in %s, mined at height %d: %d output(s), %d of them carrying data already sent\n", id, tx.TxID(), height, t.Count, used)
	}
	return nil
}

// refusedSweep says, loudly, that the network will never mine tx, the sweep
// of a tree of count funding outputs: an input of it is spent by another
// transaction. A funding output is signed for only under this home's key,
// so one spent elsewhere was spent by another copy of this home, and no
// sweep of the whole tree can mine beside that spend. earlier is set for a
// sweep an earlier run made: this run never took its fee coin, so a fee
// coin the refusal does not show spent is in neither the pool nor, once the
// record goes, the home, and is named with how to take it back.
func (g *global) refusedSweep(tree string, count uint32, tx *transaction.Transaction, err error, earlier bool) {
	lines := []string{"THE NETWORK REFUSED THE SWEEP: IT RETRACTS NOTHING",
		fmt.Sprintf("Sweep %s of tree %s: %s", tx.TxID(), tree, strings.TrimPrefix(err.Error(), "sweep: "))}
	funding := func(outpoint string) bool {
		txid, vout, _ := strings.Cut(outpoint, ".")
		v, perr := strconv.ParseUint(vout, 10, 32)
		return perr == nil && txid == tree && v < uint64(count)
	}
	var se *nodeapi.SpentError
	if errors.As(err, &se) && funding(se.Outpoint) {
		lines = append(lines, "Funding output "+se.Outpoint+" of the tree is spent by "+se.By+", which only this home's key can sign: another copy of this home spent it. That transaction retracts what the outputs it spends carried once it is mined, and no sweep of every output of the tree can mine beside it.")
	} else {
		lines = append(lines, "Until a sweep of the tree is mined, a receiver that checks spends (-unspent) still accepts what its unspent outputs carried. Run beefcat sweep -yes again to sweep it with a new transaction.")
	}
	for _, in := range tx.Inputs {
		if in.SourceTXID == nil {
			continue
		}
		fee := fmt.Sprintf("%s.%d", in.SourceTXID, in.SourceTxOutIndex)
		if !earlier || funding(fee) {
			continue
		}
		if se != nil && se.Outpoint == fee {
			lines = append(lines, "Its fee coin "+fee+" is spent by "+se.By+".")
			continue
		}
		lines = append(lines, "Its fee coin "+fee+" is not in the pool: if no other transaction has spent it, beefcat fund -n 0 -txid "+in.SourceTXID.String()+" takes it back.")
	}
	g.banner(red, lines...)
}
