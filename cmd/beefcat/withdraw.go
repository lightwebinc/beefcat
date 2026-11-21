package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"

	"github.com/lightwebinc/beefcat/internal/peerpay"
)

const withdrawHelp = `usage: beefcat withdraw [-sats N] [-messagebox URL] -yes TO

Pay coin out of the home's pool. TO is one of:

  an address               a plain payment to it
  an identity key          a BRC-29 payment only that key's wallet can spend,
                           handed to its PeerPay inbox at -messagebox
  peerpay:KEY?url=URL      the same, with the message box in the link

A payment to an identity key is locked to a key derived for this one
payment, so the recipient's wallet needs the derivation to see it. beefcat
puts that, with the payment and its proofs, in the recipient's payment
inbox, encrypted to the recipient. It also writes the same notice to
payments/<txid>.json in the home before anything is broadcast, so a
delivery that fails can be repeated with -redeliver TXID and no coin is
ever stranded.

Without -sats, the largest coin in the pool is paid out whole, less the
miner fee. Funding outputs already minted are postage, not coin: they are
not withdrawn (see beefcat sweep).

The payment is broadcast, then waited for up to -wait for the block that
mines it. One not mined by then can still mine: its change is held, and
the next fund, sweep or withdraw puts it in the pool once it has mined. A
payment the network refuses, because a coin it spends is spent by another
transaction, never mines: a banner says so, its change is not booked, and
a withdraw still waiting for it exits nonzero.

Nothing is spent without -yes.`

// notice is what a withdrawal keeps of itself: enough to deliver it again.
type notice struct {
	Txid       string `json:"txid"`
	To         string `json:"to"`
	Satoshis   uint64 `json:"satoshis"`
	Recipient  string `json:"recipientIdentityKey,omitempty"`
	Sender     string `json:"senderIdentityKey,omitempty"`
	Prefix     string `json:"derivationPrefix,omitempty"`
	Suffix     string `json:"derivationSuffix,omitempty"`
	MessageBox string `json:"messageBox,omitempty"`
	// BeefHex is the payment as Atomic BEEF, with its proofs.
	BeefHex   string    `json:"beef"`
	Delivered bool      `json:"delivered,omitempty"`
	At        time.Time `json:"at"`
}

// deliver puts a payment in its recipient's inbox; tests replace it.
var deliver = func(ctx context.Context, box string, w wallet.Interface, m peerpay.Message) error {
	return peerpay.Send(ctx, box, w, m)
}

// randomToken is a BRC-29 derivation part. It is padded base64: a wallet
// re-encodes it canonically, and an unpadded string would become another
// key id on the far side.
func randomToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b[:])
}

func cmdWithdraw(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("withdraw", withdrawHelp)
	fs.StringVar(&g.fSettle, "settle", "", "settlement leg (default: the home's, else arcade:<network>)")
	sats := fs.Uint64("sats", 0, "satoshis to pay; 0 pays the largest coin whole, less the fee")
	box := fs.String("messagebox", "", "the recipient's message box server, for a payment to an identity key")
	redeliver := fs.String("redeliver", "", "deliver again the notice of an earlier withdrawal, by its txid; spends nothing")
	yes := fs.Bool("yes", false, "actually pay; without it nothing is spent")
	wait := fs.Duration("wait", 30*time.Minute, "how long to wait for the block that mines the payment")
	pos, err := g.parse(fs, args)
	if err != nil {
		return err
	}
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	sg := h.e.Signer()
	if *redeliver != "" {
		if len(pos) > 0 {
			return usage("-redeliver takes no destination")
		}
		n, path, err := g.readNotice(*redeliver)
		if err != nil {
			return err
		}
		if n.Recipient == "" {
			return usage("withdrawal %s paid an address: there is nothing to deliver", n.Txid)
		}
		if *box != "" {
			n.MessageBox = *box
		}
		// The notice's box, or the one given now, is held to the same rule
		// as a new withdrawal's: nothing goes to a message box in the clear.
		if err := peerpay.CheckBox(n.MessageBox); err != nil {
			return usage("%v", err)
		}
		return g.deliverNotice(ctx, sg, n, path)
	}
	if len(pos) != 1 {
		return usage("withdraw [-sats N] [-messagebox URL] -yes TO (an address, an identity key, or a peerpay: link)")
	}
	to := pos[0]
	var dest *script.Script
	n := notice{To: to}
	switch {
	case strings.HasPrefix(to, "peerpay:"):
		key, linkBox, err := peerpay.ParseURI(to)
		if err != nil {
			return usage("%v", err)
		}
		n.Recipient = key
		if *box == "" {
			*box = linkBox
		}
	case peerpay.CheckKey(to) == nil:
		n.Recipient = to
	default:
		addr, err := script.NewAddressFromString(to)
		if err != nil {
			return usage("%q is not an address, an identity key or a peerpay: link", to)
		}
		if mainnet := strings.HasPrefix(to, "1"); mainnet != (g.cfg.Network == "main") {
			return usage("address %s is not a %s address", to, g.cfg.Network)
		}
		if dest, err = p2pkh.Lock(addr); err != nil {
			return err
		}
	}
	if n.Recipient != "" {
		if *box == "" {
			return usage("a payment to an identity key needs its message box: -messagebox URL, or a peerpay: link that names one")
		}
		if err := peerpay.CheckBox(*box); err != nil {
			return usage("%v", err)
		}
		if n.Recipient == sg.IdentityHex() {
			return usage("that is this home's own identity key")
		}
		n.MessageBox, n.Sender = *box, sg.IdentityHex()
		n.Prefix, n.Suffix = randomToken(), randomToken()
		if dest, err = sg.PaymentDestination(ctx, n.Recipient, n.Prefix, n.Suffix); err != nil {
			return err
		}
	}
	// The amount: what was asked, or the largest spendable coin less the fee.
	fees := mint.DefaultFees
	e, err := g.engine(ctx, h, *wait)
	if err != nil {
		return err
	}
	if err := e.collect(ctx); err != nil {
		return err
	}
	var largest uint64
	for _, o := range h.e.Pool.Outputs() {
		if o.Spendable(e.payer.Tip) && o.Satoshis > largest {
			largest = o.Satoshis
		}
	}
	pay, need := *sats, *sats+fees.Floor
	if pay == 0 {
		if largest <= fees.Floor {
			return fmt.Errorf("the pool holds no coin larger than the %d sat fee (pool: %d output(s), %d sat)", fees.Floor, h.e.Pool.Count(), h.e.Pool.Balance())
		}
		pay, need = largest-fees.Floor, largest
	}
	if need > largest {
		return fmt.Errorf("the pool's largest coin is %d sat; %d sat and the %d sat fee do not fit in it", largest, pay, fees.Floor)
	}
	how := "a plain payment"
	if n.Recipient != "" {
		how = "a BRC-29 payment, delivered to its PeerPay inbox at " + n.MessageBox
	}
	if !*yes {
		fmt.Fprintf(g.stdout, "would pay %d sat to %s on network %s: %s\nnothing was spent; run again with -yes\n", pay, to, g.cfg.Network, how)
		return nil
	}
	fee, err := e.payer.TakeAtLeast(ctx, need)
	if err != nil {
		return fmt.Errorf("withdraw: %w", err)
	}
	change, err := sg.FundScript()
	if err != nil {
		e.payer.GiveBack()
		return err
	}
	tx, err := mint.Payment(ctx, dest, pay, fee, change, fees)
	if err != nil {
		e.payer.GiveBack()
		return fmt.Errorf("withdraw: %w", err)
	}
	beef, err := funding.BEEF(tx)
	if err != nil {
		e.payer.GiveBack()
		return fmt.Errorf("withdraw: %w", err)
	}
	// The notice is on disk before the payment is on the network.
	n.Txid, n.Satoshis, n.BeefHex, n.At = tx.TxID().String(), tx.Outputs[0].Satoshis, hex.EncodeToString(beef), time.Now().UTC()
	path, err := g.writeNotice(n)
	if err != nil {
		e.payer.GiveBack()
		return err
	}
	g.say("payment %s: %d sat to %s, broadcasting via %s (%d bytes)", n.Txid, n.Satoshis, to, e.payer.Settler.Name(), tx.Size())
	if err := e.payer.Settler.Submit(ctx, tx); err != nil {
		e.payer.GiveBack()
		_ = os.Remove(path)
		return fmt.Errorf("withdraw: the network did not take the payment, and nothing was spent: %w", err)
	}
	fmt.Fprintf(g.stdout, "paid %d sat to %s\n  txid   %s\n  notice %s\n", n.Satoshis, to, n.Txid, path)
	var derr error
	if n.Recipient != "" {
		derr = g.deliverNotice(ctx, sg, n, path)
	}
	mp, height, err := e.payer.Await(ctx, "payment", tx)
	switch {
	case errors.Is(err, producer.ErrRefused):
		// It never mines: its change is not booked, and its coin goes back
		// to the pool only if the refusal did not show it spent by another
		// transaction (producer.Payer.Await takes such a coin out).
		_, back := e.payer.Reserved(fee)
		e.payer.GiveBack()
		coin := "The coin it spent is spent by another transaction and is out of the pool."
		if back {
			coin = "The coin it spent is back in the pool."
		}
		g.refusedPayment(n.Txid, err, "No change from it is booked. "+coin)
		return errors.New("withdraw: the network refused the payment")
	case err != nil:
		// Not mined in time, or the chain view could not say: the payment
		// can still mine, so its change is held (engine.collect).
		e.payer.Change(tx, 0, nil)
		g.say("the payment is broadcast and not known to be mined (%v); its change is held until a later fund, sweep or withdraw finds it mined", err)
		return derr
	}
	e.payer.Change(tx, height, mp)
	fmt.Fprintf(g.stdout, "  mined at height %d; pool %d output(s), %d sat\n", height, h.e.Pool.Count(), h.e.Pool.Balance())
	return derr
}

// refusedPayment says, loudly, that the network will never mine payment
// txid: an input of it is spent by another transaction. err is the
// refusal, and pool what became of the coin.
func (g *global) refusedPayment(txid string, err error, pool string) {
	lines := []string{"THE NETWORK REFUSED THE PAYMENT: IT WILL NEVER BE MINED",
		fmt.Sprintf("Payment %s: %s", txid, strings.TrimPrefix(err.Error(), "payment: ")),
		"The recipient receives nothing. " + pool}
	if n, path, nerr := g.readNotice(txid); nerr == nil && n.Recipient != "" {
		if n.Delivered {
			lines = append(lines, "Its notice reached the payment inbox of "+n.Recipient+": tell the recipient that the payment is void.")
		}
		lines = append(lines, "Its notice stays in "+path+"; do not deliver it again.")
	}
	g.banner(red, lines...)
}

// deliverNotice hands a BRC-29 payment to its recipient's inbox and marks
// the notice delivered.
func (g *global) deliverNotice(ctx context.Context, w wallet.Interface, n notice, path string) error {
	beef, err := hex.DecodeString(n.BeefHex)
	if err != nil {
		return fmt.Errorf("notice %s: %w", path, err)
	}
	m, err := peerpay.Seal(ctx, w, "beefcat", n.Recipient, peerpay.NewToken(beef, n.Satoshis, n.Prefix, n.Suffix))
	if err != nil {
		return err
	}
	if err := deliver(ctx, n.MessageBox, w, m); err != nil {
		g.banner(yellow, "THE PAYMENT IS MADE BUT NOT DELIVERED",
			fmt.Sprintf("%d sat went to a key derived for %s, and the message box did not take the notice: %v", n.Satoshis, n.Recipient, err),
			"The recipient's wallet cannot see the payment until it has the notice. Deliver it again with: beefcat withdraw -redeliver "+n.Txid,
			"The notice is also in "+path+" and can be handed over by any other means.")
		return errors.New("withdraw: the notice was not delivered")
	}
	n.Delivered = true
	if _, err := g.writeNotice(n); err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "  delivered to the payment inbox of %s at %s\n", n.Recipient, n.MessageBox)
	return nil
}

func (g *global) noticePath(txid string) string {
	return filepath.Join(g.home, "payments", txid+".json")
}

func (g *global) writeNotice(n notice) (string, error) {
	dir := filepath.Join(g.home, "payments")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return "", err
	}
	path := g.noticePath(n.Txid)
	return path, os.WriteFile(path, append(raw, '\n'), 0o600)
}

func (g *global) readNotice(txid string) (notice, string, error) {
	var n notice
	if len(txid) != 64 || strings.Trim(txid, "0123456789abcdef") != "" {
		return n, "", usage("-redeliver %q is not a transaction id", txid)
	}
	path := g.noticePath(txid)
	raw, err := os.ReadFile(path)
	if err != nil {
		return n, "", fmt.Errorf("no notice of withdrawal %s in this home: %w", txid, err)
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return n, "", fmt.Errorf("%s: %w", path, err)
	}
	return n, path, nil
}

// paymentOf is a notice's payment transaction, for tests and inspection.
func paymentOf(n notice) (*transaction.Transaction, error) {
	b, err := hex.DecodeString(n.BeefHex)
	if err != nil {
		return nil, err
	}
	return transaction.NewTransactionFromBEEF(b)
}
