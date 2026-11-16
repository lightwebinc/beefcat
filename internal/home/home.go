// Package home is a sender's directory: its identity and coin pool
// (bcommon's bwallet), its settings, and the funding trees its carriers
// spend from.
package home

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
)

// Config is what init writes and every command reads. A flag overrides a
// field for one run.
type Config struct {
	// Network is main, test or regtest.
	Network string `json:"network"`
	// Headers is the header source every proof is checked against.
	Headers string `json:"headers,omitempty"`
	// Chain is the chain view: transactions, proofs and spends.
	Chain string `json:"chain,omitempty"`
	// Settle is the settlement leg funding trees and sweeps are mined
	// through.
	Settle string `json:"settle,omitempty"`
	// RPC is a node's JSON-RPC, for coinbase on a regtest chain you run.
	RPC string `json:"rpc,omitempty"`
}

// Defaults fills what a network implies.
func (c *Config) Defaults() {
	if c.Network == "" {
		c.Network = "main"
	}
	if c.Network == "main" || c.Network == "test" {
		if c.Headers == "" {
			c.Headers = "woc:" + c.Network
		}
		if c.Chain == "" {
			c.Chain = "woc:" + c.Network
		}
		if c.Settle == "" {
			c.Settle = "arcade:" + c.Network
		}
	}
}

// Pending is a funding tree signed for a coin and not yet recorded as
// mined: what producer.Trees.Prepare was given.
type Pending struct {
	Tree funding.Tree   `json:"tree"`
	Coin bwallet.Output `json:"coin"`
	// Asked counts the commands that found the tree unknown and its coin
	// unspent.
	Asked int `json:"asked,omitempty"`
}

// Sweep is a sweep of one tree's outputs.
type Sweep struct {
	Tree   string `json:"tree"`
	Txid   string `json:"txid"`
	RawHex string `json:"rawHex"`
	// Height is set once the sweep mined.
	Height uint32 `json:"height,omitempty"`
}

// State is the home's record of its funding trees.
type State struct {
	Identity string         `json:"identity"`
	Trees    []funding.Tree `json:"trees,omitempty"`
	Pending  []Pending      `json:"pending,omitempty"`
	Sweeps   []Sweep        `json:"sweeps,omitempty"`
	// Sent counts the carriers and data bytes this home has sent.
	SentCarriers uint64 `json:"sentCarriers,omitempty"`
	SentBytes    uint64 `json:"sentBytes,omitempty"`

	path string
}

const (
	configFile = "config.json"
	stateFile  = "state.json"
)

// LoadConfig reads the home's settings; a home without any is empty.
func LoadConfig(dir string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(filepath.Join(dir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("%s: %w", configFile, err)
	}
	return c, nil
}

// SaveConfig writes the home's settings.
func SaveConfig(dir string, c Config) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, configFile), append(raw, '\n'))
}

// LoadState reads the home's state for identity (hex), or starts an empty
// one. The caller holds the lock.
func LoadState(dir, identity string) (*State, error) {
	s := &State{Identity: identity, path: filepath.Join(dir, stateFile)}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, fmt.Errorf("%s: %w", stateFile, err)
	}
	if s.Identity != identity {
		return nil, fmt.Errorf("%s belongs to identity %s, not this home's %s", stateFile, s.Identity, identity)
	}
	return s, nil
}

// Save writes the state, atomically.
func (s *State) Save() error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomic(s.path, append(raw, '\n'))
}

// Remaining is how many carriers the mined trees can still fund.
func (s *State) Remaining() uint64 {
	var n uint64
	for i := range s.Trees {
		if s.Trees[i].BumpHex != "" {
			n += uint64(s.Trees[i].Remaining())
		}
	}
	return n
}

// NextTree is the first mined tree with an output left, or nil.
func (s *State) NextTree() *funding.Tree {
	for i := range s.Trees {
		if t := &s.Trees[i]; t.BumpHex != "" && t.Remaining() > 0 {
			return t
		}
	}
	return nil
}

// Tree is the recorded tree txid, or nil.
func (s *State) Tree(txid string) *funding.Tree {
	for i := range s.Trees {
		if s.Trees[i].Txid == txid {
			return &s.Trees[i]
		}
	}
	return nil
}

// DropPending forgets the pending record of txid.
func (s *State) DropPending(txid string) {
	kept := s.Pending[:0]
	for _, p := range s.Pending {
		if p.Tree.Txid != txid {
			kept = append(kept, p)
		}
	}
	s.Pending = kept
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
