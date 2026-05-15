package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// stubLookup is a deterministic SSHConfigLookup for tests — never touches
// the operator's ~/.ssh/config.
type stubLookup map[string]map[string]string

func (s stubLookup) Get(alias, key string) string {
	if m, ok := s[alias]; ok {
		return m[key]
	}
	return ""
}

// passthroughConn is an in-memory sshConn that "tunnels" by dialing the
// target host directly — the test is asserting Resolver pooling +
// concurrency, not the data plane, so no real proxy is needed. Keepalive
// pings are counted; failure can be triggered by setting keepFailAt.
type passthroughConn struct {
	keepCount  atomic.Int64
	keepFailAt int64
	closed     atomic.Bool
}

func (f *passthroughConn) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if f.closed.Load() {
		return nil, errors.New("fake: closed")
	}
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

func (f *passthroughConn) SendRequest(name string, wantReply bool, payload []byte) (bool, []byte, error) {
	n := f.keepCount.Add(1)
	if f.keepFailAt > 0 && n >= f.keepFailAt {
		return false, nil, errors.New("fake: keepalive broken pipe")
	}
	return true, nil, nil
}

func (f *passthroughConn) Close() error {
	f.closed.Store(true)
	return nil
}

// TestHTTPClient_EmptyAlias_ReturnsDefaultClient covers the zero-overhead
// path: callers without ssh_jump must get http.DefaultClient back so they
// preserve every existing keepalive / pooling semantic. The same path
// must also work on a nil Resolver so callers don't need per-callsite
// nil-check helpers.
func TestHTTPClient_EmptyAlias_ReturnsDefaultClient(t *testing.T) {
	r := NewResolver(nil, nil)
	t.Cleanup(func() { _ = r.Close() })

	hc, err := r.HTTPClient(context.Background(), "")
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	if hc != http.DefaultClient {
		t.Errorf("HTTPClient(\"\") should be pointer-equal to http.DefaultClient")
	}

	// Nil receiver path — must not panic and must return DefaultClient.
	var nilResolver *Resolver
	hc, err = nilResolver.HTTPClient(context.Background(), "anything")
	if err != nil {
		t.Fatalf("nil-receiver HTTPClient: %v", err)
	}
	if hc != http.DefaultClient {
		t.Errorf("nil-receiver HTTPClient should be pointer-equal to http.DefaultClient")
	}
}

// TestHTTPClient_UnknownAlias_FailsFast verifies aliases not present
// inline AND not in the ssh_config lookup yield an error from HTTPClient
// itself — so a typo surfaces before the first request.
func TestHTTPClient_UnknownAlias_FailsFast(t *testing.T) {
	r := NewResolver(nil, stubLookup{})
	t.Cleanup(func() { _ = r.Close() })

	_, err := r.HTTPClient(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for unknown alias")
	}
}

// TestResolver_OneConnPerAlias verifies the "1 SSH conn per alias even
// under N concurrent callers" contract. The fake dialer counts how many
// times the resolver actually opens a new sshConn.
func TestResolver_OneConnPerAlias(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(target.Close)

	var dialCount atomic.Int64
	dialer := func(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (sshConn, error) {
		dialCount.Add(1)
		return &passthroughConn{}, nil
	}
	r := newResolverForTest(
		map[string]BastionConfig{"b1": {Host: "h", User: "u"}},
		stubLookup{},
		dialer,
	)
	t.Cleanup(func() { _ = r.Close() })

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			hc, err := r.HTTPClient(context.Background(), "b1")
			if err != nil {
				t.Errorf("HTTPClient: %v", err)
				return
			}
			resp, err := hc.Get(target.URL)
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	if got := r.DialCount(); got != 1 {
		t.Errorf("DialCount: got %d, want 1", got)
	}
	if got := dialCount.Load(); got != 1 {
		t.Errorf("dialer invocations: got %d, want 1", got)
	}
}

// TestResolver_EvictionUnderLoad triggers a keepalive failure that evicts
// the entry; the next HTTPClient call must redial. The architect-mandated
// getter contract means in-flight goroutines hold a captured reference to
// the old client and complete without panic; the new caller observes the
// fresh dial.
func TestResolver_EvictionUnderLoad(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(target.Close)

	var dialCount atomic.Int64
	dialer := func(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (sshConn, error) {
		n := dialCount.Add(1)
		fake := &passthroughConn{}
		if n == 1 {
			// First dial: keepalive fails after one tick.
			fake.keepFailAt = 1
		}
		return fake, nil
	}

	r := newResolverForTest(
		map[string]BastionConfig{
			"b1": {Host: "h", User: "u", KeepaliveInterval: 20 * time.Millisecond},
		},
		stubLookup{},
		dialer,
	)
	t.Cleanup(func() { _ = r.Close() })

	hc, err := r.HTTPClient(context.Background(), "b1")
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	if resp, err := hc.Get(target.URL); err != nil {
		t.Fatalf("first Get: %v", err)
	} else {
		resp.Body.Close()
	}
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("after first call, dialCount: got %d, want 1", got)
	}

	// Wait for keepalive to fire and evict.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.DialCount() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := r.DialCount(); got != 0 {
		t.Fatalf("after eviction, DialCount: got %d, want 0", got)
	}

	hc2, err := r.HTTPClient(context.Background(), "b1")
	if err != nil {
		t.Fatalf("HTTPClient after eviction: %v", err)
	}
	if resp, err := hc2.Get(target.URL); err != nil {
		t.Fatalf("second Get: %v", err)
	} else {
		resp.Body.Close()
	}
	if got := dialCount.Load(); got < 2 {
		t.Errorf("after eviction+redial, dialCount: got %d, want >=2", got)
	}
}

// TestResolver_Close_NoGoroutineLeak verifies Close cancels keepalive
// goroutines and waits for them to exit, with no goroutines escaping.
// The test goes through getOrDial directly to avoid net/http persistConn
// goroutines leaking from an httptest server — those are unrelated to
// the resolver contract under test.
func TestResolver_Close_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	dialer := func(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (sshConn, error) {
		return &passthroughConn{}, nil
	}
	r := newResolverForTest(
		map[string]BastionConfig{
			"b1": {Host: "h", User: "u", KeepaliveInterval: 1 * time.Hour},
			"b2": {Host: "h", User: "u", KeepaliveInterval: 1 * time.Hour},
		},
		stubLookup{},
		dialer,
	)

	for _, alias := range []string{"b1", "b2"} {
		if _, err := r.getOrDial(context.Background(), alias); err != nil {
			t.Fatalf("getOrDial %s: %v", alias, err)
		}
	}
	if got := r.DialCount(); got != 2 {
		t.Fatalf("DialCount: got %d, want 2", got)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestResolver_HTTPClientAfterClose verifies the resolver returns an
// error from getOrDial (surfaced via Transport.DialContext) instead of
// silently producing a dead client when called post-Close.
func TestResolver_HTTPClientAfterClose(t *testing.T) {
	dialer := func(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (sshConn, error) {
		return &passthroughConn{}, nil
	}
	r := newResolverForTest(
		map[string]BastionConfig{"b": {Host: "h", User: "u"}},
		stubLookup{},
		dialer,
	)
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	hc, err := r.HTTPClient(context.Background(), "b")
	if err != nil {
		t.Fatalf("HTTPClient post-close: %v", err)
	}
	_, err = hc.Get("http://127.0.0.1:1/")
	if err == nil {
		t.Fatal("expected error from Get after Close")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error should mention closed resolver, got: %v", err)
	}
}

// TestResolver_ProxyJump_TwoHop verifies that a target with proxy_jump
// triggers a recursive dial of the proxy first, with the proxy's
// *ssh.Client passed as the underlying dialer for the target.
func TestResolver_ProxyJump_TwoHop(t *testing.T) {
	var dials []string
	var underlyingTypes []string
	var mu sync.Mutex
	dialer := func(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (sshConn, error) {
		mu.Lock()
		dials = append(dials, cfg.Alias)
		switch underlying.(type) {
		case *net.Dialer:
			underlyingTypes = append(underlyingTypes, "net.Dialer")
		case *passthroughConn:
			underlyingTypes = append(underlyingTypes, "passthroughConn")
		default:
			underlyingTypes = append(underlyingTypes, fmt.Sprintf("%T", underlying))
		}
		mu.Unlock()
		return &passthroughConn{}, nil
	}

	r := newResolverForTest(
		map[string]BastionConfig{
			"target":  {Host: "target.internal", User: "u", ProxyJump: "bastion"},
			"bastion": {Host: "bastion.example.com", User: "u"},
		},
		stubLookup{},
		dialer,
	)
	t.Cleanup(func() { _ = r.Close() })

	if _, err := r.getOrDial(context.Background(), "target"); err != nil {
		t.Fatalf("getOrDial target: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(dials) != 2 {
		t.Fatalf("expected 2 dials (bastion + target), got %d: %v", len(dials), dials)
	}
	if dials[0] != "bastion" || dials[1] != "target" {
		t.Errorf("dial order: got %v, want [bastion target]", dials)
	}
	if underlyingTypes[0] != "net.Dialer" {
		t.Errorf("first dial underlying: got %s, want net.Dialer", underlyingTypes[0])
	}
	if underlyingTypes[1] != "passthroughConn" {
		t.Errorf("second dial underlying: got %s, want passthroughConn", underlyingTypes[1])
	}
}

// TestResolver_ProxyJump_SharedIntermediate verifies that two targets
// sharing the same proxy bastion open exactly ONE *ssh.Client to the
// shared bastion — the pool's key is the alias, so the recursion hits
// the cache on the second target's dial.
func TestResolver_ProxyJump_SharedIntermediate(t *testing.T) {
	var dialCount atomic.Int64
	dialer := func(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (sshConn, error) {
		dialCount.Add(1)
		return &passthroughConn{}, nil
	}

	r := newResolverForTest(
		map[string]BastionConfig{
			"seq-1":   {Host: "seq-1.internal", User: "u", ProxyJump: "bastion"},
			"seq-2":   {Host: "seq-2.internal", User: "u", ProxyJump: "bastion"},
			"bastion": {Host: "bastion.example.com", User: "u"},
		},
		stubLookup{},
		dialer,
	)
	t.Cleanup(func() { _ = r.Close() })

	if _, err := r.getOrDial(context.Background(), "seq-1"); err != nil {
		t.Fatalf("getOrDial seq-1: %v", err)
	}
	if _, err := r.getOrDial(context.Background(), "seq-2"); err != nil {
		t.Fatalf("getOrDial seq-2: %v", err)
	}

	// Expected dials: bastion (first time), seq-1 (uses cached bastion),
	// seq-2 (uses cached bastion). Total = 3, not 4.
	if got := dialCount.Load(); got != 3 {
		t.Errorf("dialer invocations: got %d, want 3 (1 shared bastion + 2 targets)", got)
	}
	if got := r.DialCount(); got != 3 {
		t.Errorf("DialCount: got %d, want 3", got)
	}
}

// TestResolver_ProxyJump_RuntimeCycleGuard verifies the defensive
// runtime cycle detection. config.Load() rejects cycles at startup, but
// the resolver guards against hand-built inline maps that smuggle a
// cycle past validation.
func TestResolver_ProxyJump_RuntimeCycleGuard(t *testing.T) {
	dialer := func(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (sshConn, error) {
		return &passthroughConn{}, nil
	}
	r := newResolverForTest(
		map[string]BastionConfig{
			"a": {Host: "a", User: "u", ProxyJump: "b"},
			"b": {Host: "b", User: "u", ProxyJump: "a"},
		},
		stubLookup{},
		dialer,
	)
	t.Cleanup(func() { _ = r.Close() })

	_, err := r.getOrDial(context.Background(), "a")
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle: %v", err)
	}
}

// TestResolver_ProxyJump_CloseAllLevels verifies that Close() cancels
// and closes every level of a chain — no level leaks.
func TestResolver_ProxyJump_CloseAllLevels(t *testing.T) {
	defer goleak.VerifyNone(t)

	dialer := func(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (sshConn, error) {
		return &passthroughConn{}, nil
	}
	r := newResolverForTest(
		map[string]BastionConfig{
			"a": {Host: "a", User: "u", ProxyJump: "b", KeepaliveInterval: 1 * time.Hour},
			"b": {Host: "b", User: "u", ProxyJump: "c", KeepaliveInterval: 1 * time.Hour},
			"c": {Host: "c", User: "u", KeepaliveInterval: 1 * time.Hour},
		},
		stubLookup{},
		dialer,
	)

	if _, err := r.getOrDial(context.Background(), "a"); err != nil {
		t.Fatalf("getOrDial: %v", err)
	}
	if got := r.DialCount(); got != 3 {
		t.Fatalf("DialCount after 3-hop dial: got %d, want 3", got)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestResolver_ProxyJump_SSHConfigDirective verifies that ProxyJump
// directives in ~/.ssh/config are honored when the inline bastion does
// not set proxy_jump explicitly.
func TestResolver_ProxyJump_SSHConfigDirective(t *testing.T) {
	var dials []string
	var mu sync.Mutex
	dialer := func(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (sshConn, error) {
		mu.Lock()
		dials = append(dials, cfg.Alias)
		mu.Unlock()
		return &passthroughConn{}, nil
	}

	r := newResolverForTest(
		map[string]BastionConfig{
			"target": {Host: "target.internal", User: "u"},
			// "bastion" is ssh_config-only (no inline entry).
		},
		stubLookup{
			"target":  {"ProxyJump": "bastion"},
			"bastion": {"HostName": "bastion.example.com", "User": "u"},
		},
		dialer,
	)
	t.Cleanup(func() { _ = r.Close() })

	if _, err := r.getOrDial(context.Background(), "target"); err != nil {
		t.Fatalf("getOrDial target: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(dials) != 2 {
		t.Fatalf("expected 2 dials, got %d: %v", len(dials), dials)
	}
	if dials[0] != "bastion" || dials[1] != "target" {
		t.Errorf("dial order: got %v, want [bastion target]", dials)
	}
}

func TestExpandPath(t *testing.T) {
	t.Setenv("FOO", "bar")
	t.Run("env_var", func(t *testing.T) {
		got, err := expandPath("$FOO/baz")
		if err != nil {
			t.Fatalf("expandPath: %v", err)
		}
		if got != "bar/baz" {
			t.Errorf("got %q, want %q", got, "bar/baz")
		}
	})
	t.Run("absolute_unchanged", func(t *testing.T) {
		got, err := expandPath("/tmp/x")
		if err != nil {
			t.Fatalf("expandPath: %v", err)
		}
		if got != "/tmp/x" {
			t.Errorf("got %q, want %q", got, "/tmp/x")
		}
	})
	t.Run("tilde_user_rejected", func(t *testing.T) {
		_, err := expandPath("~deploy/.ssh/x")
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("empty_unchanged", func(t *testing.T) {
		got, err := expandPath("")
		if err != nil {
			t.Fatalf("expandPath: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}
