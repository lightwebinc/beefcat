// Package peerpay delivers a BRC-29 payment to its recipient the way
// PeerPay does: the payment token, encrypted to the recipient, put in the
// recipient's payment_inbox at a message box server (BRC-33).
//
// It follows @bsv/message-box-client: the token's JSON, the message id as
// an HMAC of that JSON, and the body as {"encryptedMessage": base64}, each
// under the protocol [1, "messagebox"], key id "1", with the recipient as
// counterparty. Every request is mutually authenticated (BRC-103/104) by
// go-sdk's AuthFetch under the sender's wallet. A box that asks to be paid
// for delivery is refused rather than paid: nothing here spends.
package peerpay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	authhttp "github.com/bsv-blockchain/go-sdk/auth/clients/authhttp"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"
)

// Inbox is the message box PeerPay reads payments from.
const Inbox = "payment_inbox"

// Protocol is the message box protocol every id and body is derived under.
var Protocol = wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "messagebox"}

// ErrPayment is a box that wants paying for delivery.
var ErrPayment = errors.New("peerpay: the message box asks to be paid for delivery, which beefcat does not do")

// Token is PeerPay's payment token. Transaction is the payment as Atomic
// BEEF, written as a JSON array of numbers, as the TypeScript client writes
// a byte array.
type Token struct {
	CustomInstructions Instructions `json:"customInstructions"`
	Transaction        []uint16     `json:"transaction"`
	Amount             uint64       `json:"amount"`
}

// Instructions are the BRC-29 derivation the recipient re-derives its key
// from, with the sender's identity key, which the message box supplies.
type Instructions struct {
	DerivationPrefix string `json:"derivationPrefix"`
	DerivationSuffix string `json:"derivationSuffix"`
}

// NewToken is the token for a payment whose output 0 pays sats under the
// derivation.
func NewToken(beef []byte, sats uint64, prefix, suffix string) Token {
	tx := make([]uint16, len(beef))
	for i, b := range beef {
		tx[i] = uint16(b)
	}
	return Token{CustomInstructions: Instructions{DerivationPrefix: prefix, DerivationSuffix: suffix}, Transaction: tx, Amount: sats}
}

// Message is what is put in the recipient's box.
type Message struct {
	Recipient string
	ID        string
	Body      string
}

// Seal makes the message for a token: its id and its encrypted body.
func Seal(ctx context.Context, w wallet.Interface, originator, recipientHex string, t Token) (Message, error) {
	cp, err := bwallet.Counterparty(recipientHex)
	if err != nil {
		return Message{}, err
	}
	plain, err := json.Marshal(t)
	if err != nil {
		return Message{}, err
	}
	args := wallet.EncryptionArgs{ProtocolID: Protocol, KeyID: "1", Counterparty: cp}
	mac, err := w.CreateHMAC(ctx, wallet.CreateHMACArgs{EncryptionArgs: args, Data: IDInput(plain)}, originator)
	if err != nil {
		return Message{}, fmt.Errorf("peerpay: message id: %w", err)
	}
	enc, err := w.Encrypt(ctx, wallet.EncryptArgs{EncryptionArgs: args, Plaintext: plain}, originator)
	if err != nil {
		return Message{}, fmt.Errorf("peerpay: encrypt: %w", err)
	}
	body, err := json.Marshal(map[string]string{"encryptedMessage": base64.StdEncoding.EncodeToString(enc.Ciphertext)})
	if err != nil {
		return Message{}, err
	}
	return Message{Recipient: recipientHex, ID: hex.EncodeToString(mac.HMAC[:]), Body: string(body)}, nil
}

// IDInput is what a message's id is the HMAC of. The TypeScript client is
// handed the token as a string and hashes that string's JSON form, quotes
// and escapes included, while it encrypts the string itself.
func IDInput(token []byte) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(string(token))
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// noPay is the sender's wallet with action creation refused, so AuthFetch's
// answer to a 402 (pay and retry) cannot spend anything.
type noPay struct{ wallet.Interface }

func (noPay) CreateAction(context.Context, wallet.CreateActionArgs, string) (*wallet.CreateActionResult, error) {
	return nil, ErrPayment
}

// maxAnswer bounds what a server's answer may be, and sendTimeout how long
// one delivery may take.
const (
	maxAnswer   = 1 << 20
	sendTimeout = 30 * time.Second
)

// Send puts m in the recipient's payment inbox at the message box server.
func Send(ctx context.Context, base string, w wallet.Interface, m Message) error {
	if err := CheckBox(base); err != nil {
		return err
	}
	// A payment is already broadcast when its notice is sent, so a box that
	// takes the connection and never answers must not hold the command.
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	f := authhttp.New(noPay{w}, authhttp.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	body, err := json.Marshal(map[string]any{"message": map[string]any{
		"recipient": m.Recipient, "messageBox": Inbox, "messageId": m.ID, "body": m.Body,
	}})
	if err != nil {
		return err
	}
	resp, err := f.Fetch(ctx, strings.TrimRight(base, "/")+"/sendMessage", &authhttp.SimplifiedFetchRequestOptions{
		Method: http.MethodPost, Headers: map[string]string{"Content-Type": "application/json"}, Body: body,
	})
	if err != nil {
		if errors.Is(err, ErrPayment) {
			return ErrPayment
		}
		return fmt.Errorf("message box: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return fmt.Errorf("message box: %w", err)
	}
	var st struct {
		Status      string `json:"status"`
		Code        string `json:"code"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &st)
	if resp.StatusCode == http.StatusPaymentRequired {
		return ErrPayment
	}
	if resp.StatusCode != http.StatusOK || st.Status != "success" {
		return fmt.Errorf("message box: status %d %s %s", resp.StatusCode, st.Code, st.Description)
	}
	return nil
}

// ParseURI reads a PeerPay URI, peerpay:<identity key>?url=<message box>,
// and returns the identity key in hex and the message box, which is empty
// when the URI names none.
func ParseURI(s string) (keyHex, box string, err error) {
	rest, ok := strings.CutPrefix(s, "peerpay:")
	if !ok {
		return "", "", errors.New("not a peerpay: URI")
	}
	key, query, _ := strings.Cut(rest, "?")
	if err := CheckKey(key); err != nil {
		return "", "", err
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return "", "", fmt.Errorf("peerpay URI: %w", err)
	}
	if box = q.Get("url"); box != "" {
		if err := CheckBox(box); err != nil {
			return "", "", err
		}
	}
	return key, box, nil
}

// CheckKey refuses anything but a canonical compressed public key in
// lowercase hex. One key has one spelling: a message box files a payment
// under the recipient as it is written, and a key is compared as a string.
func CheckKey(keyHex string) error {
	b, err := hex.DecodeString(keyHex)
	if err != nil || len(b) != 33 || keyHex != strings.ToLower(keyHex) {
		return fmt.Errorf("%q is not an identity key (66 lowercase hex characters)", keyHex)
	}
	if _, err := guard.ParsePubKey(b); err != nil {
		return fmt.Errorf("%q is not a public key: %v", keyHex, err)
	}
	return nil
}

// CheckBox refuses a message box that is not an https URL, or http on
// loopback.
func CheckBox(box string) error {
	u, err := url.Parse(box)
	if err != nil || u.Host == "" {
		return fmt.Errorf("message box %q is not a URL", box)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		return nil
	}
	return fmt.Errorf("message box %q must be https", box)
}
