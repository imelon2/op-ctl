package probe

import (
	"context"
	"sync"
	"time"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
	"op-ctl/internal/sshtunnel"
)

// AdminProbe is the per-backend result of one admin_nodeInfo call attempt
// against an execution-layer endpoint.
//
// Symmetry with Probe (consensus opp2p_self) is intentional: callers can
// reason about both fan-outs the same way. Result is non-nil iff OK is
// true. Latency is the wall-clock duration of the HTTP round trip whether
// OK or not — useful for rendering "ERR (5.0s)" on slow failures.
type AdminProbe struct {
	Backend string
	OK      bool
	Latency time.Duration
	Result  *elnode.NodeInfo
	Err     error
}

// AdminProbeAll fans out one goroutine per backend against
// backend.ExecutionRPCURL and returns a slice in the same order as the
// input.
//
// Each call uses context.WithTimeout(parent, perCallTimeout) so a single
// slow backend cannot block the others. Output is positionally aligned
// with bs, never reordered.
//
// Resolver routing mirrors ProbeAll: a non-nil resolver + a non-empty
// ssh_jump on the backend routes the call through the corresponding
// bastion; otherwise the underlying elnode.Self call uses the default
// transport.
func AdminProbeAll(parent context.Context, perCallTimeout time.Duration, maxRetries int, resolver *sshtunnel.Resolver, bs []config.Backend) []AdminProbe {
	out := make([]AdminProbe, len(bs))
	var wg sync.WaitGroup
	for i, b := range bs {
		i, b := i, b
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = adminProbeOneWithRetry(parent, perCallTimeout, maxRetries, resolver, b)
		}()
	}
	wg.Wait()
	return out
}

// adminProbeOneWithRetry mirrors probeOneWithRetry for the
// execution-layer admin_nodeInfo call. Retry semantics match: up to
// maxRetries+1 attempts, retryBackoff between, parent-ctx aware.
func adminProbeOneWithRetry(parent context.Context, perCallTimeout time.Duration, maxRetries int, resolver *sshtunnel.Resolver, b config.Backend) AdminProbe {
	var (
		lastErr error
		lastLat time.Duration
		info    *elnode.NodeInfo
	)
	attempts := maxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-parent.Done():
				return AdminProbe{Backend: b.Name, OK: false, Latency: lastLat, Err: parent.Err()}
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
		info, lastLat, lastErr = elnode.Self(ctx, hc, b.ExecutionRPCURL)
		cancel()
		if lastErr == nil {
			return AdminProbe{Backend: b.Name, OK: true, Latency: lastLat, Result: info}
		}
	}
	return AdminProbe{Backend: b.Name, OK: false, Latency: lastLat, Err: lastErr}
}
