// Package sshtunnel routes op-ctl's outbound HTTP RPC traffic through
// SSH bastions. The package contract:
//
//   - The Resolver owns at most one *ssh.Client per bastion alias and
//     re-uses it across every HTTPClient(ctx, alias) call. The expensive
//     resource (the SSH connection) is pooled; the cheap *http.Client
//     wrapper is rebuilt per call so each Transport.DialContext closure
//     re-reads the current cached client through a getter (not a captured
//     pointer). This is the concurrency-correctness contract for
//     eviction-under-load: in-flight dials retain the *ssh.Client they
//     captured locally; the *next* call after eviction triggers a redial.
//
//   - HTTPClient("") returns http.DefaultClient (pointer-equal) — backends
//     without a bastion pay zero overhead and keep their existing
//     keep-alive semantics.
//
//   - Resolver is safe for concurrent use. Close() cancels every
//     keepalive goroutine, waits, then closes the underlying SSH
//     connections; subsequent HTTPClient calls return an error.
//
// op-ctl-specific design notes (not goals):
//
//   - HTTPClient returns a fresh *http.Client per call. op-ctl issues
//     short-lived one-shot RPCs from TUI refreshes, so transport-pool
//     reuse across calls is not a goal — only the SSH tunnel reuse
//     matters. The reused tunnel multiplexes all RPC TCP streams as
//     SSH "direct-tcpip" channels.
package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const defaultKeepaliveInterval = 30 * time.Second

// dialerFunc is the dial seam used by Resolver. Production wires it to
// realDialer (→ dialSSHThrough); tests inject fakes so unit coverage
// does not require a real SSH daemon.
//
// `underlying` is the dialer used for the TCP leg — *net.Dialer for the
// first hop, a previously-dialed *ssh.Client for any subsequent hop in a
// ProxyJump chain. Both satisfy the netDialer interface in dialer.go.
type dialerFunc func(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (sshConn, error)

// sshConn is the surface the Resolver actually exercises on an *ssh.Client.
// Defined as an interface so tests can substitute a fake.
type sshConn interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	SendRequest(name string, wantReply bool, payload []byte) (bool, []byte, error)
	Close() error
}

// realDialer adapts dialSSHThrough (which returns *ssh.Client) to the
// sshConn interface used by Resolver.
func realDialer(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (sshConn, error) {
	return dialSSHThrough(ctx, underlying, cfg, lookup)
}

// Compile-time assertion: *ssh.Client satisfies sshConn.
var _ sshConn = (*ssh.Client)(nil)

// Resolver pools one SSH tunnel per bastion alias.
type Resolver struct {
	inline map[string]BastionConfig
	lookup SSHConfigLookup
	dialer dialerFunc

	mu      sync.Mutex
	clients map[string]*entry
	wg      sync.WaitGroup
	closed  bool
}

type entry struct {
	client    sshConn
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

func (e *entry) closeClient() error {
	e.closeOnce.Do(func() {
		e.closeErr = e.client.Close()
	})
	return e.closeErr
}

// NewResolver builds a Resolver with production defaults: inline bastion
// definitions from config.toml + the kevinburke/ssh_config lookup. A nil
// lookup defaults to DefaultSSHConfigLookup so tests can pass nil for
// "no ssh_config available".
func NewResolver(inline map[string]BastionConfig, lookup SSHConfigLookup) *Resolver {
	if lookup == nil {
		lookup = DefaultSSHConfigLookup{}
	}
	return &Resolver{
		inline:  inline,
		lookup:  lookup,
		dialer:  realDialer,
		clients: make(map[string]*entry),
	}
}

// newResolverForTest is the test-only seam — accepts a synthetic dialer.
func newResolverForTest(inline map[string]BastionConfig, lookup SSHConfigLookup, dialer dialerFunc) *Resolver {
	if lookup == nil {
		lookup = DefaultSSHConfigLookup{}
	}
	return &Resolver{
		inline:  inline,
		lookup:  lookup,
		dialer:  dialer,
		clients: make(map[string]*entry),
	}
}

// HTTPClient returns an *http.Client that routes through the bastion
// named by alias. Calling with alias == "" (or invoking on a nil
// Resolver) returns http.DefaultClient pointer-equal so direct-connect
// backends pay zero overhead — this is the single dispatch seam every
// caller relies on, no per-caller nil-check helpers required.
//
// The returned client's Transport.DialContext re-reads the current cached
// *ssh.Client per dial via the getter, so eviction events do not strand
// in-flight callers and never return a closed connection to a fresh
// caller.
func (r *Resolver) HTTPClient(ctx context.Context, alias string) (*http.Client, error) {
	if r == nil || alias == "" {
		return http.DefaultClient, nil
	}

	// Fail-fast: ensure the alias is at least known. The dial itself
	// is deferred until first request so a typo or DNS issue with one
	// bastion does not block startup of unrelated backends.
	if _, err := r.resolveCfg(alias); err != nil {
		return nil, err
	}

	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			client, err := r.getOrDial(dialCtx, alias)
			if err != nil {
				return nil, err
			}
			return client.DialContext(dialCtx, network, addr)
		},
		// Disable proxy lookup: we already are the proxy.
		Proxy: nil,
	}
	return &http.Client{Transport: transport}, nil
}

// DialCount returns the number of pooled SSH connections. Test hook —
// asserts "one bastion per alias" under concurrent load.
func (r *Resolver) DialCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.clients)
}

// Close cancels every keepalive goroutine, waits for them to exit,
// then closes all underlying SSH connections. After Close the Resolver
// rejects further HTTPClient calls for any non-empty alias.
func (r *Resolver) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	snapshot := r.clients
	r.clients = make(map[string]*entry)
	r.mu.Unlock()

	for _, e := range snapshot {
		e.cancel()
	}
	r.wg.Wait()

	var errs []error
	for alias, e := range snapshot {
		if err := e.closeClient(); err != nil && !isBenignCloseErr(err) {
			errs = append(errs, fmt.Errorf("close %s: %w", alias, err))
		}
	}
	return errors.Join(errs...)
}

// isBenignCloseErr filters out shutdown-race errors that are expected
// when closing a multi-hop chain in arbitrary order. If the parent
// *ssh.Client is closed first, its child's underlying direct-tcpip
// channel becomes invalid — the child's subsequent Close then surfaces
// io.EOF or net.ErrClosed. Both indicate the peer already tore the
// connection down; nothing was leaked.
func isBenignCloseErr(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}

// resolveCfg returns the BastionConfig the resolver will dial with for
// alias. Inline entries win; an alias present in only ~/.ssh/config is
// represented by a near-empty BastionConfig (Host/User filled in by
// dialSSHThrough from the lookup).
//
// ProxyJump precedence: inline > ssh_config. When inline omits proxy_jump,
// ssh_config's ProxyJump directive (if any) is folded in here so the
// chain walker sees one consistent value per alias regardless of source.
// OpenSSH's comma-separated multi-hop ProxyJump form is reduced to its
// first hop — the chain walker handles further hops by following each
// alias's own proxy_jump in turn.
func (r *Resolver) resolveCfg(alias string) (BastionConfig, error) {
	cfg, inlineOK := r.inline[alias]
	if !inlineOK {
		if r.lookup.Get(alias, "HostName") == "" {
			return BastionConfig{}, fmt.Errorf("ssh bastion %q: unknown alias (not in inline config or ~/.ssh/config)", alias)
		}
		cfg = BastionConfig{Alias: alias}
	} else {
		cfg.Alias = alias
	}
	if strings.TrimSpace(cfg.ProxyJump) == "" {
		if pj := r.lookup.Get(alias, "ProxyJump"); strings.TrimSpace(pj) != "" {
			cfg.ProxyJump = firstHop(pj)
		}
	}
	return cfg, nil
}

// firstHop returns the first comma-separated alias from an OpenSSH-style
// ProxyJump value, trimmed of whitespace. Mirrors the helper in
// internal/config; duplicated to keep sshtunnel free of a back-import.
func firstHop(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, ","); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

// getOrDial returns the pooled *ssh.Client for alias, dialing on first
// use. Walks the alias's proxy_jump chain so chained bastions all share
// the resolver's pool (one *ssh.Client per alias, even when chains
// overlap). Concurrent callers race once on the first dial: the loser
// closes its tunnel and returns the winner's.
func (r *Resolver) getOrDial(ctx context.Context, alias string) (sshConn, error) {
	return r.getOrDialChain(ctx, alias, map[string]bool{})
}

// getOrDialChain is getOrDial with cycle detection. `stack` tracks the
// aliases currently being resolved on this call's recursion path; an
// alias appearing twice on the same path is an unrecoverable cycle.
//
// config.Load() rejects proxy_jump cycles at startup, so this guard is
// defense-in-depth — useful when sshtunnel is reused outside op-ctl with
// a hand-built inline map.
func (r *Resolver) getOrDialChain(ctx context.Context, alias string, stack map[string]bool) (sshConn, error) {
	if stack[alias] {
		return nil, fmt.Errorf("ssh proxy_jump cycle detected at %q", alias)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, fmt.Errorf("ssh bastion resolver: closed")
	}
	if e, ok := r.clients[alias]; ok {
		r.mu.Unlock()
		return e.client, nil
	}
	r.mu.Unlock()

	cfg, err := r.resolveCfg(alias)
	if err != nil {
		return nil, err
	}

	var underlying netDialer = &net.Dialer{}
	if parent := strings.TrimSpace(cfg.ProxyJump); parent != "" {
		stack[alias] = true
		parentConn, err := r.getOrDialChain(ctx, parent, stack)
		delete(stack, alias)
		if err != nil {
			return nil, fmt.Errorf("ssh bastion %q proxy_jump %q: %w", alias, parent, err)
		}
		underlying = parentConn
	}

	client, err := r.dialer(ctx, underlying, cfg, r.lookup)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = client.Close()
		return nil, fmt.Errorf("ssh bastion resolver: closed")
	}
	if e, ok := r.clients[alias]; ok {
		r.mu.Unlock()
		_ = client.Close() // lost the race
		return e.client, nil
	}
	keepCtx, cancel := context.WithCancel(context.Background())
	e := &entry{client: client, cancel: cancel}
	r.clients[alias] = e
	interval := cfg.KeepaliveInterval
	if interval <= 0 {
		interval = defaultKeepaliveInterval
	}
	r.wg.Add(1)
	go r.runKeepalive(keepCtx, alias, e, interval)
	r.mu.Unlock()
	return client, nil
}

// runKeepalive pings the SSH server on a tick so NAT/firewall idle
// timeouts do not silently drop the long-lived multiplexed connection.
// On any SendRequest failure the entry is evicted (next caller redials)
// and the underlying client is closed via the entry's sync.Once.
func (r *Resolver) runKeepalive(ctx context.Context, alias string, e *entry, interval time.Duration) {
	defer r.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, _, err := e.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				r.mu.Lock()
				if cur, ok := r.clients[alias]; ok && cur == e {
					delete(r.clients, alias)
				}
				r.mu.Unlock()
				_ = e.closeClient()
				return
			}
		}
	}
}
