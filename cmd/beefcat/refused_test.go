package main

import (
	"context"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/testchain"

	"github.com/lightwebinc/beefcat/internal/peerpay"
	"github.com/lightwebinc/beefcat/wire"
)

// other is the transaction a competing spend is made by.
var other = strings.Repeat("ab", 32)

// spendFirstInput marks the first input of the transaction txid spent by
// other, as a competing spend the network took first would.
func spendFirstInput(t *testing.T, c *testchain.Chain, txid string) {
	t.Helper()
	tx := c.Tx(txid)
	if tx == nil {
		t.Fatalf("the chain does not hold %s", txid)
	}
	in := tx.Inputs[0]
	c.SpendElsewhere(in.SourceTXID.String(), in.SourceTxOutIndex, other)
}

// spendNextElsewhere does spendFirstInput to the next transaction the chain
// takes and holds unmined, as soon as it is there.
func spendNextElsewhere(t *testing.T, c *testchain.Chain) {
	t.Helper()
	known := map[string]bool{}
	for _, id := range c.Txids() {
		known[id] = true
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			for _, id := range c.Txids() {
				if !known[id] && !c.Mined(id) {
					in := c.Tx(id).Inputs[0]
					c.SpendElsewhere(in.SourceTXID.String(), in.SourceTxOutIndex, other)
					return
				}
			}
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() {
		close(done)
		wg.Wait()
	})
}

// pool is the home's coin pool as it is on disk.
func pool(t *testing.T, h string) []bwallet.Output {
	t.Helper()
	e, err := bwallet.Open(h, wire.Profile)
	if err != nil {
		t.Fatal(err)
	}
	return e.Pool.Outputs()
}

func address(t *testing.T) string {
	t.Helper()
	key, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), false)
	if err != nil {
		t.Fatal(err)
	}
	return addr.AddressString
}

func txidIn(t *testing.T, stdout []byte) string {
	t.Helper()
	i := strings.Index(string(stdout), "txid")
	if i < 0 {
		t.Fatalf("no txid in %q", stdout)
	}
	return strings.Fields(string(stdout)[i:])[1]
}

// loud asserts a banner with headline and returns stderr with the
// banner's lines joined again, as they were before it wrapped them.
func loud(t *testing.T, stderr, headline string) string {
	t.Helper()
	text := strings.ReplaceAll(stderr, "\n!!  ", " ")
	if !strings.Contains(text, headline) || !strings.Contains(stderr, strings.Repeat("!", 76)) {
		t.Fatalf("no %q banner:\n%s", headline, stderr)
	}
	return text
}

// A payment the network refuses while withdraw waits for its block is
// reported in a banner, -q or not, and exits nonzero. It never mines, so
// its change is not booked, and the coin a competing spend took is out of
// the pool.
func TestWithdrawRefusedIsLoud(t *testing.T) {
	r := newRig(t, 1)
	r.chain.SetHold(true)
	spendNextElsewhere(t, r.chain)
	code, o, e := cat(t, nil, "withdraw", "-home", r.home, "-q", "-sats", "5000", "-wait", "1m", "-yes", address(t))
	if code != exitError {
		t.Fatalf("exit %d, want %d\n%s%s", code, exitError, o, e)
	}
	e = loud(t, e, "THE NETWORK REFUSED THE PAYMENT: IT WILL NEVER BE MINED")
	txid := txidIn(t, o)
	if !strings.Contains(e, "spent by "+other) || !strings.Contains(e, "out of the pool") {
		t.Fatalf("the banner does not say why, or what became of the coin:\n%s", e)
	}
	spent := r.chain.Tx(txid).Inputs[0]
	for _, c := range pool(t, r.home) {
		if c.Unproven || c.TxID == txid {
			t.Fatalf("the refused payment's change is booked: %+v", c)
		}
		if c.TxID == spent.SourceTXID.String() && c.Vout == spent.SourceTxOutIndex {
			t.Fatalf("the coin another transaction spent is back in the pool: %s", c.Outpoint())
		}
	}
}

// A refused payment to an identity key whose notice was delivered says
// so, and that the recipient is owed nothing.
func TestWithdrawRefusedAfterDeliveryIsLoud(t *testing.T) {
	r := newRig(t, 1)
	r.chain.SetHold(true)
	rk, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	rhex := hex.EncodeToString(rk.PubKey().Compressed())
	old := deliver
	deliver = func(_ context.Context, _ string, _ wallet.Interface, _ peerpay.Message) error {
		// The payment is on the network, and a competing spend of its coin
		// gets there first.
		for _, id := range r.chain.Txids() {
			if !r.chain.Mined(id) {
				spendFirstInput(t, r.chain, id)
			}
		}
		return nil
	}
	defer func() { deliver = old }()
	link := "peerpay:" + rhex + "?url=https%3A%2F%2Fbox.example.com"
	code, o, e := cat(t, nil, "withdraw", "-home", r.home, "-q", "-sats", "4000", "-wait", "1m", "-yes", link)
	if code != exitError {
		t.Fatalf("exit %d, want %d\n%s%s", code, exitError, o, e)
	}
	e = loud(t, e, "THE NETWORK REFUSED THE PAYMENT: IT WILL NEVER BE MINED")
	if !strings.Contains(e, "Its notice reached the payment inbox of "+rhex) || !strings.Contains(e, "do not deliver it again") {
		t.Fatalf("the banner does not say the notice was delivered:\n%s", e)
	}
}

// A payment not mined within -wait can still mine: its change is held, and
// a later command puts it in the pool once the payment has mined.
func TestWithdrawPendingChangeIsCollected(t *testing.T) {
	r := newRig(t, 1)
	r.chain.SetHold(true)
	code, o, e := cat(t, nil, "withdraw", "-home", r.home, "-sats", "5000", "-wait", "100ms", "-yes", address(t))
	if code != 0 || strings.Contains(e, "REFUSED") {
		t.Fatalf("a payment not mined in time: %d\n%s%s", code, o, e)
	}
	txid := txidIn(t, o)
	change := func() (n int, unproven bool) {
		for _, c := range pool(t, r.home) {
			if c.TxID == txid {
				n, unproven = n+1, unproven || c.Unproven
			}
		}
		return n, unproven
	}
	if n, unproven := change(); n != 1 || !unproven {
		t.Fatalf("the change is not held: %d coin(s), unproven %v", n, unproven)
	}
	// Not mined yet: a later command leaves it held.
	if code, o, e := cat(t, nil, "fund", "-home", r.home, "-n", "0"); code != 0 {
		t.Fatalf("fund: %d\n%s%s", code, o, e)
	}
	if n, unproven := change(); n != 1 || !unproven {
		t.Fatalf("the change of an unmined payment: %d coin(s), unproven %v", n, unproven)
	}
	r.chain.Mine()
	code, o, e = cat(t, nil, "fund", "-home", r.home, "-n", "0")
	if code != 0 {
		t.Fatalf("fund after the payment mined: %d\n%s%s", code, o, e)
	}
	if n, unproven := change(); n != 1 || unproven {
		t.Fatalf("the change of a mined payment: %d coin(s), unproven %v", n, unproven)
	}
	if !strings.Contains(e, "change from "+txid+": mined at height") {
		t.Fatalf("fund does not say it collected the change:\n%s", e)
	}
}

// A payment not mined within -wait and refused afterwards is found by the
// next command, which says so loudly and takes its held change out of the
// pool.
func TestWithdrawRefusedLaterIsLoud(t *testing.T) {
	r := newRig(t, 1)
	r.chain.SetHold(true)
	code, o, e := cat(t, nil, "withdraw", "-home", r.home, "-sats", "5000", "-wait", "100ms", "-yes", address(t))
	if code != 0 {
		t.Fatalf("withdraw: %d\n%s%s", code, o, e)
	}
	txid := txidIn(t, o)
	spendFirstInput(t, r.chain, txid)
	code, o, e = cat(t, nil, "fund", "-home", r.home, "-q", "-n", "0")
	if code != 0 {
		t.Fatalf("fund: %d\n%s%s", code, o, e)
	}
	e = loud(t, e, "THE NETWORK REFUSED THE PAYMENT: IT WILL NEVER BE MINED")
	if !strings.Contains(e, txid) || !strings.Contains(e, "is taken out") {
		t.Fatalf("the banner does not name the payment or its change:\n%s", e)
	}
	for _, c := range pool(t, r.home) {
		if c.TxID == txid {
			t.Fatalf("the refused payment's change is still in the pool: %+v", c)
		}
	}
}

// sweptBy is the sweep status names for the home's one tree, or "".
func sweptBy(t *testing.T, h string) (string, string) {
	t.Helper()
	_, o, _ := cat(t, nil, "status", "-home", h)
	for _, l := range strings.Split(string(o), "\n") {
		if !strings.HasPrefix(l, "tree ") {
			continue
		}
		if _, rest, ok := strings.Cut(l, "swept by "); ok {
			return strings.Fields(rest)[0], l
		}
		return "", l
	}
	t.Fatalf("no tree in status:\n%s", o)
	return "", ""
}

// A sweep broadcast by an earlier run and refused since is reported in a
// banner, and its record goes, so the next sweep makes a new one. The tree
// stays closed to sends.
func TestSweepRefusedIsLoud(t *testing.T) {
	r := newRig(t, 1)
	r.chain.SetHold(true)
	code, o, e := cat(t, nil, "sweep", "-home", r.home, "-wait", "100ms", "-yes")
	if code != exitError || !strings.Contains(e, "the sweep is broadcast") {
		t.Fatalf("a sweep not mined in time: %d\n%s%s", code, o, e)
	}
	sweep, _ := sweptBy(t, r.home)
	if sweep == "" {
		t.Fatal("the sweep is not recorded")
	}
	spendFirstInput(t, r.chain, sweep)
	code, o, e = cat(t, nil, "sweep", "-home", r.home, "-q", "-yes")
	if code != exitError {
		t.Fatalf("exit %d, want %d\n%s%s", code, exitError, o, e)
	}
	e = loud(t, e, "THE NETWORK REFUSED THE SWEEP: IT RETRACTS NOTHING")
	if !strings.Contains(e, sweep) || !strings.Contains(e, "beefcat sweep -yes again") {
		t.Fatalf("the banner does not name the sweep or what to do:\n%s", e)
	}
	if by, line := sweptBy(t, r.home); by != "" || !strings.Contains(line, " 0 of 1 left") {
		t.Fatalf("after the refusal: %s", line)
	}
}

// A sweep the network refuses while it is waited for is reported in a
// banner, and undone.
func TestSweepRefusedWhileWaitingIsLoud(t *testing.T) {
	r := newRig(t, 1)
	r.chain.SetHold(true)
	spendNextElsewhere(t, r.chain)
	code, o, e := cat(t, nil, "sweep", "-home", r.home, "-q", "-wait", "1m", "-yes")
	if code != exitError {
		t.Fatalf("exit %d, want %d\n%s%s", code, exitError, o, e)
	}
	loud(t, e, "THE NETWORK REFUSED THE SWEEP: IT RETRACTS NOTHING")
	if by, line := sweptBy(t, r.home); by != "" || !strings.Contains(line, " 1 of 1 left") {
		t.Fatalf("after the refusal: %s", line)
	}
}
