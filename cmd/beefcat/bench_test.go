package main

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/lightwebinc/bcommon/headers"

	"github.com/lightwebinc/beefcat/recv"
)

// BenchmarkReceive verifies a stream of 4 MiB carriers held in memory, so
// what it measures is the verifier: no network and no disk. One worker is
// the cost of a carrier on one core; the default is what a host gives.
func BenchmarkReceive(b *testing.B) {
	const carriers = 32
	_, url := benchChain(b)
	r := &rig{t: b, url: url}
	r.home = r.newHome(carriers)
	stream := r.stream(random(carriers << 22))
	hc, err := headers.NewSource(url)
	if err != nil {
		b.Fatal(err)
	}
	for _, bc := range []struct {
		name    string
		workers int
	}{{"1-worker", 1}, {"4-workers", 4}, {"default", 0}} {
		b.Run(bc.name, func(b *testing.B) {
			b.SetBytes(carriers << 22)
			for range b.N {
				if _, err := recv.Receive(context.Background(), bytes.NewReader(stream), io.Discard, recv.Options{Headers: hc, Workers: bc.workers}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
