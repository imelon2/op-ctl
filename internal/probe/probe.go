// Package probe orchestrates parallel opp2p_self calls across all
// configured backends.
//
// The TUI imports this package (not opnode) so the layer rule "TUI never
// originates RPC calls" holds: the TUI requests a Probe batch, the probe
// package fans out to opnode.
package probe

import (
	"context"
	"sync"
	"time"

	"op-ctl/internal/config"
	"op-ctl/internal/opnode"
	"op-ctl/internal/sshtunnel"
)

// Probe is the per-backend result of one opp2p_self call attempt.
//
// Result is non-nil iff OK is true. Err carries the failure cause when OK
// is false (timeout, RPC error, decode failure, etc.). Latency is the
// wall-clock duration of the HTTP round trip whether OK or not — useful for
// rendering "ERR (5.0s)" on slow failures.
type Probe struct {
	Backend string
	OK      bool
	Latency time.Duration
	Result  *opnode.PeerInfo
	Err     error
}

// retryBackoff is the fixed delay between retry attempts. Short
// enough to keep the namespace command snappy on transient failures;
// long enough that a flapping endpoint isn't hammered. A package
// var (not const) so tests can shorten it to keep their wall-clock
// budget tight without exposing the knob to operators.
var retryBackoff = 500 * time.Millisecond

// ProbeAll fans out one goroutine per backend and returns a slice in the
// same order as the input.
//
// Each call uses context.WithTimeout(parent, perCallTimeout) so a single
// slow backend cannot block the others. maxRetries is the number of
// *additional* attempts after the first (so maxRetries=3 means up to
// 4 total attempts per backend); a value of 0 disables retries. The
// loop exits early when parent ctx is cancelled. Output is
// positionally aligned with bs, never reordered.
//
// When resolver is non-nil and a backend declares ssh_jump, the per-call
// *http.Client routes through the corresponding bastion. A nil resolver
// (or a backend without ssh_jump) falls back to http.DefaultClient.
func ProbeAll(parent context.Context, perCallTimeout time.Duration, maxRetries int, resolver *sshtunnel.Resolver, bs []config.Backend) []Probe {
	out := make([]Probe, len(bs))
	var wg sync.WaitGroup
	for i, b := range bs {
		i, b := i, b
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = probeOneWithRetry(parent, perCallTimeout, maxRetries, resolver, b)
		}()
	}
	wg.Wait()
	return out
}

// probeOneWithRetry runs up to maxRetries+1 attempts of opp2p_self
// against a single backend. The latest attempt's latency and error
// are reported; a successful attempt short-circuits the loop.
func probeOneWithRetry(parent context.Context, perCallTimeout time.Duration, maxRetries int, resolver *sshtunnel.Resolver, b config.Backend) Probe {
	var (
		lastErr error
		lastLat time.Duration
		info    *opnode.PeerInfo
	)
	attempts := maxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-parent.Done():
				return Probe{Backend: b.Name, OK: false, Latency: lastLat, Err: parent.Err()}
			case <-time.After(retryBackoff):
			}
		}
		ctx, cancel := context.WithTimeout(parent, perCallTimeout)
		hc, herr := resolver.HTTPClient(ctx, b.SSHJump)
		if herr != nil {
			cancel()
			lastErr = herr
			continue
		}
		info, lastLat, lastErr = opnode.Self(ctx, hc, b.ConsensusRPCURL)
		cancel()
		if lastErr == nil {
			return Probe{Backend: b.Name, OK: true, Latency: lastLat, Result: info}
		}
	}
	return Probe{Backend: b.Name, OK: false, Latency: lastLat, Err: lastErr}
}

