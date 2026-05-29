package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return p
}

func TestLoad_SingleBackend(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
execution_rpc_url = "http://host:4000"
consensus_rpc_url = "http://host:4004"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(c.Backends); got != 1 {
		t.Fatalf("backends count: got %d, want 1", got)
	}
	b, ok := c.Backends["sequencer"]
	if !ok {
		t.Fatal("missing sequencer backend")
	}
	if b.Name != "sequencer" {
		t.Errorf("Name: got %q, want %q", b.Name, "sequencer")
	}
	if b.ConsensusRPCURL != "http://host:4004" {
		t.Errorf("ConsensusRPCURL: got %q", b.ConsensusRPCURL)
	}
}

func TestBackendList_PreservesSourceOrder(t *testing.T) {
	// Order chosen to be NOT alphabetically sorted: sequencer, archive, full-1.
	// Alphabetical would be archive, full-1, sequencer.
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[backends.archive]
consensus_rpc_url = "http://b:2"

[backends.full-1]
consensus_rpc_url = "http://c:3"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := []string{}
	for _, b := range c.BackendList() {
		got = append(got, b.Name)
	}
	want := []string{"sequencer", "archive", "full-1"}
	if len(got) != len(want) {
		t.Fatalf("length: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order at %d: got %q, want %q (full got=%v)", i, got[i], want[i], got)
		}
	}
}

func TestLoad_NoBackendsTable(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[global]
foo = "bar"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for missing [backends]")
	}
	if !strings.Contains(err.Error(), "no [backends.*] tables found") {
		t.Errorf("error message: %v", err)
	}
}

func TestLoad_EmptyBackendsTable(t *testing.T) {
	// [backends] header with no children — equivalent to no backends.
	p := writeTemp(t, "config.toml", `
[backends]
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for empty [backends]")
	}
	if !strings.Contains(err.Error(), "no [backends.*] tables found") {
		t.Errorf("error message: %v", err)
	}
}

func TestLoad_MissingConsensusRPCURL(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
execution_rpc_url = "http://host:4000"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for missing consensus_rpc_url")
	}
	if !strings.Contains(err.Error(), "missing required consensus_rpc_url") {
		t.Errorf("error message: %v", err)
	}
	if !strings.Contains(err.Error(), `"sequencer"`) {
		t.Errorf("error should name backend: %v", err)
	}
}

func TestLoad_GlobalNamespaceTimeout(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[global]
namespace_timeout = "12s"

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.Global.NamespaceTimeout, 12*time.Second; got != want {
		t.Errorf("NamespaceTimeout: got %v, want %v", got, want)
	}
}

func TestLoad_GlobalNamespaceTimeout_Absent(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Global.NamespaceTimeout != 0 {
		t.Errorf("NamespaceTimeout absent should be zero, got %v", c.Global.NamespaceTimeout)
	}
}

func TestLoad_GlobalNamespaceTimeout_Invalid(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[global]
namespace_timeout = "not-a-duration"

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid timeout")
	}
	if !strings.Contains(err.Error(), "global.namespace_timeout") {
		t.Errorf("error should mention global.namespace_timeout: %v", err)
	}
}

func TestLoad_GlobalNamespaceTimeout_NonPositive(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[global]
namespace_timeout = "0s"

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for zero timeout")
	}
	if !strings.Contains(err.Error(), "must be positive") {
		t.Errorf("error should mention positive: %v", err)
	}
}

func TestLoad_BastionInline_Valid(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://seq-1.testnet.paychain:9545"
execution_rpc_url = "http://seq-1.testnet.paychain:8545"
ssh_jump = "ops-bastion"

[bastions.ops-bastion]
host = "bastion.example.com"
user = "deploy"
identity_file = "~/.ssh/id_ed25519"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, ok := c.Bastions["ops-bastion"]
	if !ok {
		t.Fatal("missing ops-bastion entry")
	}
	if b.Name != "ops-bastion" {
		t.Errorf("Name: got %q", b.Name)
	}
	if b.Host != "bastion.example.com" {
		t.Errorf("Host: got %q", b.Host)
	}
	if b.Port != 22 {
		t.Errorf("Port default: got %d, want 22", b.Port)
	}
	if b.KnownHosts != "~/.ssh/known_hosts" {
		t.Errorf("KnownHosts default: got %q", b.KnownHosts)
	}
	if c.Backends["seq1"].SSHJump != "ops-bastion" {
		t.Errorf("SSHJump: got %q", c.Backends["seq1"].SSHJump)
	}
}

func TestLoad_BastionInline_MissingHost(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://x:1"

[bastions.b]
user = "deploy"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for bastion missing host")
	}
	if !strings.Contains(err.Error(), "missing required host") {
		t.Errorf("error: %v", err)
	}
}

func TestLoad_BastionInline_MissingUser(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://x:1"

[bastions.b]
host = "bastion.example.com"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for bastion missing user")
	}
	if !strings.Contains(err.Error(), "missing required user") {
		t.Errorf("error: %v", err)
	}
}

func TestLoad_BastionInline_KeepaliveDuration(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://x:1"
ssh_jump = "b"

[bastions.b]
host = "bastion.example.com"
user = "deploy"
keepalive_interval = "45s"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.Bastions["b"].KeepaliveInterval, 45*time.Second; got != want {
		t.Errorf("KeepaliveInterval: got %v, want %v", got, want)
	}
}

func TestLoad_BastionInline_KeepaliveInvalid(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://x:1"
ssh_jump = "b"

[bastions.b]
host = "bastion.example.com"
user = "deploy"
keepalive_interval = "nope"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid keepalive_interval")
	}
	if !strings.Contains(err.Error(), "keepalive_interval") {
		t.Errorf("error: %v", err)
	}
}

func TestLoad_BastionInline_TildeUserRejected(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://x:1"

[bastions.b]
host = "bastion.example.com"
user = "deploy"
identity_file = "~deploy/.ssh/id_ed25519"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for ~user identity_file")
	}
	if !strings.Contains(err.Error(), "~user form is not supported") {
		t.Errorf("error: %v", err)
	}
}

// stubSSHConfig replaces the package-level SSHConfigGet for the test's
// lifetime. Pass a map { alias -> { key -> value } }.
func stubSSHConfig(t *testing.T, entries map[string]map[string]string) {
	t.Helper()
	prev := SSHConfigGet
	SSHConfigGet = func(alias, key string) string {
		if m, ok := entries[alias]; ok {
			return m[key]
		}
		return ""
	}
	t.Cleanup(func() { SSHConfigGet = prev })
}

// TestLoad_SSHJumpUnknown verifies the resolver rejects an ssh_jump alias
// that is neither inline nor in ~/.ssh/config.
func TestLoad_SSHJumpUnknown(t *testing.T) {
	stubSSHConfig(t, nil)

	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://x:1"
ssh_jump = "no-such-alias"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for unknown ssh_jump alias")
	}
	if !strings.Contains(err.Error(), "ssh_jump references unknown alias") {
		t.Errorf("error: %v", err)
	}
	if !strings.Contains(err.Error(), "[bastions.*]") {
		t.Errorf("error should mention inline source: %v", err)
	}
	if !strings.Contains(err.Error(), "~/.ssh/config") {
		t.Errorf("error should mention ssh_config source: %v", err)
	}
}

func TestLoad_ProxyJump_Inline_Valid(t *testing.T) {
	stubSSHConfig(t, nil)

	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://127.0.0.1:9545"
ssh_jump = "seq-1"

[bastions.seq-1]
host = "seq-1.internal"
user = "deploy"
proxy_jump = "ops-bastion"

[bastions.ops-bastion]
host = "bastion.example.com"
user = "deploy"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Bastions["seq-1"].ProxyJump; got != "ops-bastion" {
		t.Errorf("ProxyJump: got %q, want ops-bastion", got)
	}
}

func TestLoad_ProxyJump_SSHConfig_Valid(t *testing.T) {
	// proxy_jump can resolve to a Host block in ~/.ssh/config.
	stubSSHConfig(t, map[string]map[string]string{
		"ops-bastion": {"HostName": "bastion.example.com", "User": "deploy"},
	})

	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://127.0.0.1:9545"
ssh_jump = "seq-1"

[bastions.seq-1]
host = "seq-1.internal"
user = "deploy"
proxy_jump = "ops-bastion"
`)
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoad_ProxyJump_UnknownParentRejected(t *testing.T) {
	stubSSHConfig(t, nil)

	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://127.0.0.1:9545"
ssh_jump = "seq-1"

[bastions.seq-1]
host = "seq-1.internal"
user = "deploy"
proxy_jump = "missing-parent"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for unknown proxy_jump parent")
	}
	if !strings.Contains(err.Error(), "proxy_jump") {
		t.Errorf("error should mention proxy_jump: %v", err)
	}
	if !strings.Contains(err.Error(), "missing-parent") {
		t.Errorf("error should name the missing alias: %v", err)
	}
}

func TestLoad_ProxyJump_DirectCycleRejected(t *testing.T) {
	stubSSHConfig(t, nil)

	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://127.0.0.1:9545"
ssh_jump = "loop"

[bastions.loop]
host = "loop.example.com"
user = "deploy"
proxy_jump = "loop"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for direct proxy_jump cycle")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle: %v", err)
	}
}

func TestLoad_ProxyJump_IndirectCycleRejected(t *testing.T) {
	stubSSHConfig(t, nil)

	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://127.0.0.1:9545"
ssh_jump = "a"

[bastions.a]
host = "a.example.com"
user = "deploy"
proxy_jump = "b"

[bastions.b]
host = "b.example.com"
user = "deploy"
proxy_jump = "a"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for indirect proxy_jump cycle")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle: %v", err)
	}
}

// TestLoad_ProxyJump_CommaInlineNormalized verifies the architect's
// defense-in-depth note: an inline `proxy_jump = "a,b"` is reduced to
// "a" at Load time so the resolver receives a single-hop value
// symmetric with the ssh_config path. The chain walker then follows
// alias `a`'s own proxy_jump for any further hops.
func TestLoad_ProxyJump_CommaInlineNormalized(t *testing.T) {
	stubSSHConfig(t, nil)

	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://127.0.0.1:9545"
ssh_jump = "target"

[bastions.target]
host = "target.internal"
user = "deploy"
proxy_jump = "a,b"

[bastions.a]
host = "a.example.com"
user = "deploy"

[bastions.b]
host = "b.example.com"
user = "deploy"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.Bastions["target"].ProxyJump, "a"; got != want {
		t.Errorf("inline ProxyJump should be normalized to first hop: got %q, want %q", got, want)
	}
}

func TestLoad_ProxyJump_MultiHopValid(t *testing.T) {
	stubSSHConfig(t, nil)

	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://127.0.0.1:9545"
ssh_jump = "innermost"

[bastions.innermost]
host = "innermost.internal"
user = "deploy"
proxy_jump = "middle"

[bastions.middle]
host = "middle.internal"
user = "deploy"
proxy_jump = "outer"

[bastions.outer]
host = "outer.example.com"
user = "deploy"
`)
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// TestLoad_SSHJumpInlineOnly verifies an alias defined inline alone is
// accepted even when ~/.ssh/config has no matching Host.
func TestLoad_SSHJumpInlineOnly(t *testing.T) {
	stubSSHConfig(t, nil)

	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://x:1"
ssh_jump = "inline-only"

[bastions.inline-only]
host = "bastion.example.com"
user = "deploy"
`)
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// TestLoad_SSHJumpShadowWarning verifies the stderr warning fires when
// an alias is defined in both inline [bastions.*] AND ~/.ssh/config.
func TestLoad_SSHJumpShadowWarning(t *testing.T) {
	stubSSHConfig(t, map[string]map[string]string{
		"shadow-bastion": {"HostName": "ssh-config-host.example.com", "User": "opsuser"},
	})

	var buf bytes.Buffer
	prev := WarningWriter
	WarningWriter = &buf
	t.Cleanup(func() { WarningWriter = prev })

	p := writeTemp(t, "config.toml", `
[backends.seq1]
consensus_rpc_url = "http://x:1"
ssh_jump = "shadow-bastion"

[bastions.shadow-bastion]
host = "bastion.example.com"
user = "deploy"
`)
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "shadow-bastion") {
		t.Errorf("warning should name alias, got: %q", out)
	}
	if !strings.Contains(out, "defined in both config.toml and ~/.ssh/config") {
		t.Errorf("warning should describe shadow case, got: %q", out)
	}
	if !strings.Contains(out, "using config.toml values") {
		t.Errorf("warning should state precedence, got: %q", out)
	}
}

func TestLoad_StateBlockTimeoutParsed(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.block]
timeout = "3s"
interval = "750ms"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.State.Block.Timeout, 3*time.Second; got != want {
		t.Errorf("State.Block.Timeout: got %v, want %v", got, want)
	}
}

// TestLoad_StateBlockIntervalParsed mirrors the timeout test to lock
// the interval-parse path independently, so a future refactor that
// drops one parse block doesn't silently regress the other.
func TestLoad_StateBlockIntervalParsed(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.block]
interval = "500ms"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.State.Block.Interval, 500*time.Millisecond; got != want {
		t.Errorf("State.Block.Interval: got %v, want %v", got, want)
	}
}

func TestLoad_StateBlockTimeout_Invalid(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.block]
timeout = "nope"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid state.block.timeout")
	}
	if !strings.Contains(err.Error(), "state.block.timeout") {
		t.Errorf("error should mention state.block.timeout: %v", err)
	}
}

func TestLoad_StateBlockTimeout_NonPositive(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.block]
timeout = "0s"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for zero state.block.timeout")
	}
	if !strings.Contains(err.Error(), "must be positive") {
		t.Errorf("error should mention positive: %v", err)
	}
}

func TestLoad_StateBlockInterval_Invalid(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.block]
interval = "nope"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid state.block.interval")
	}
	if !strings.Contains(err.Error(), "state.block.interval") {
		t.Errorf("error should mention state.block.interval: %v", err)
	}
}

func TestLoad_StateTxPoolParsed(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.txpool]
timeout = "4s"
interval = "2s"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.State.TxPool.Timeout, 4*time.Second; got != want {
		t.Errorf("State.TxPool.Timeout: got %v, want %v", got, want)
	}
	if got, want := c.State.TxPool.Interval, 2*time.Second; got != want {
		t.Errorf("State.TxPool.Interval: got %v, want %v", got, want)
	}
}

func TestLoad_StateTxPool_Absent(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.State.TxPool.Timeout != 0 || c.State.TxPool.Interval != 0 {
		t.Errorf("absent state.txpool should leave zero values, got %+v", c.State.TxPool)
	}
}

func TestLoad_StateTxPoolTimeout_Invalid(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.txpool]
timeout = "nope"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid state.txpool.timeout")
	}
	if !strings.Contains(err.Error(), "state.txpool.timeout") {
		t.Errorf("error should mention state.txpool.timeout: %v", err)
	}
}

func TestLoad_StateTxPoolTimeout_NonPositive(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.txpool]
timeout = "0s"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for zero state.txpool.timeout")
	}
	if !strings.Contains(err.Error(), "must be positive") {
		t.Errorf("error should mention positive: %v", err)
	}
}

func TestLoad_StateTxPoolInterval_Invalid(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.txpool]
interval = "nope"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid state.txpool.interval")
	}
	if !strings.Contains(err.Error(), "state.txpool.interval") {
		t.Errorf("error should mention state.txpool.interval: %v", err)
	}
}

func TestLoad_StateTxPoolDetailRefresh_Default10s(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.State.TxPool.Detail.Refresh, 10*time.Second; got != want {
		t.Errorf("unset refresh should default to 10s, got %v", got)
	}
}

func TestLoad_StateTxPoolDetailRefresh_ZeroIsManualOnly(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.txpool.detail]
refresh = "0s"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.State.TxPool.Detail.Refresh != 0 {
		t.Errorf("explicit 0s should resolve to 0 (manual only), got %v", c.State.TxPool.Detail.Refresh)
	}
}

func TestLoad_StateTxPoolDetailRefresh_ClampUnder5s(t *testing.T) {
	var buf bytes.Buffer
	prev := WarningWriter
	WarningWriter = &buf
	t.Cleanup(func() { WarningWriter = prev })

	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.txpool.detail]
refresh = "3s"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.State.TxPool.Detail.Refresh, 5*time.Second; got != want {
		t.Errorf("3s should clamp to 5s, got %v", got)
	}
	if !strings.Contains(buf.String(), "below the 5s minimum") {
		t.Errorf("expected clamp warning, got %q", buf.String())
	}
}

func TestLoad_StateTxPoolDetailRefresh_Negative_Error(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.txpool.detail]
refresh = "-1s"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for negative refresh")
	}
	if !strings.Contains(err.Error(), "state.txpool.detail.refresh") {
		t.Errorf("error should mention state.txpool.detail.refresh: %v", err)
	}
}

func TestLoad_StateTxPoolDetailRefresh_Invalid(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[state.txpool.detail]
refresh = "nope"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for unparseable duration")
	}
	if !strings.Contains(err.Error(), "state.txpool.detail.refresh") {
		t.Errorf("error should mention state.txpool.detail.refresh: %v", err)
	}
}

func TestLoad_URLsAndContracts_RelativeStateRoot(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[urls]
l1_rpc_url = "https://ethereum-sepolia-rpc.publicnode.com"
l2_rpc_url = "http://3.39.212.0:8545"

[contracts]
state_root = "state.json"

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.URLs.L1RPCURL, "https://ethereum-sepolia-rpc.publicnode.com"; got != want {
		t.Errorf("URLs.L1RPCURL: got %q, want %q", got, want)
	}
	if got, want := c.URLs.L2RPCURL, "http://3.39.212.0:8545"; got != want {
		t.Errorf("URLs.L2RPCURL: got %q, want %q", got, want)
	}
	// Relative state_root is resolved against the directory of the config
	// file (not the cwd), so the operator can invoke op-ctl from anywhere
	// without the JSON path going stale.
	wantStateRoot := filepath.Join(filepath.Dir(p), "state.json")
	if c.Contracts.StateRoot != wantStateRoot {
		t.Errorf("Contracts.StateRoot: got %q, want %q", c.Contracts.StateRoot, wantStateRoot)
	}
}

func TestLoad_ContractsStateRootAbsolute_Preserved(t *testing.T) {
	abs := "/abs/path/to/state.json"
	p := writeTemp(t, "config.toml", `
[contracts]
state_root = "`+abs+`"

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Contracts.StateRoot != abs {
		t.Errorf("absolute StateRoot should be preserved: got %q, want %q", c.Contracts.StateRoot, abs)
	}
}

func TestLoad_URLsAndContracts_AbsentLeavesZero(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.URLs.L1RPCURL != "" {
		t.Errorf("absent l1_rpc_url should leave empty string, got %q", c.URLs.L1RPCURL)
	}
	if c.URLs.L2RPCURL != "" {
		t.Errorf("absent l2_rpc_url should leave empty string, got %q", c.URLs.L2RPCURL)
	}
	if c.Contracts.StateRoot != "" {
		t.Errorf("absent state_root should leave empty string, got %q", c.Contracts.StateRoot)
	}
}

func TestLoad_NestedBackendKey(t *testing.T) {
	// Nested table under a backend should not produce a duplicate entry.
	p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"

[backends.sequencer.headers]
authorization = "Bearer x"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	names := []string{}
	for _, b := range c.BackendList() {
		names = append(names, b.Name)
	}
	if len(names) != 1 || names[0] != "sequencer" {
		t.Errorf("BackendList: got %v, want [sequencer]", names)
	}
}

// TestLoad_URLsAccepted asserts the renamed [urls] section decodes
// cleanly, including the etherscan_api_key field that joined L1/L2 RPC
// URLs in the 2026-05 schema break.
func TestLoad_URLsAccepted(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[urls]
l1_rpc_url = "https://l1.example"
l2_rpc_url = "http://l2.example:8545"
etherscan_api_key = "SOMEKEY"

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.URLs.L1RPCURL, "https://l1.example"; got != want {
		t.Errorf("URLs.L1RPCURL: got %q, want %q", got, want)
	}
	if got, want := c.URLs.L2RPCURL, "http://l2.example:8545"; got != want {
		t.Errorf("URLs.L2RPCURL: got %q, want %q", got, want)
	}
	if got, want := c.URLs.EtherscanAPIKey, "SOMEKEY"; got != want {
		t.Errorf("URLs.EtherscanAPIKey: got %q, want %q", got, want)
	}
}

// TestLoad_RPCRejected pins the hard schema break: a legacy [rpc]
// section must produce a clear Load() error instead of silently being
// ignored (which would manifest as empty L1/L2 URLs at runtime — much
// harder to debug).
func TestLoad_RPCRejected(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[rpc]
l1_rpc_url = "https://l1.example"

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error rejecting legacy [rpc] section")
	}
	if !strings.Contains(err.Error(), "legacy [rpc] section is no longer supported") {
		t.Errorf("error should mention legacy [rpc]: %v", err)
	}
	if !strings.Contains(err.Error(), "rename to [urls]") {
		t.Errorf("error should suggest rename: %v", err)
	}
}

// TestLoad_BatchAddressValidation_BatcherFromAddress asserts a
// malformed batcher_from_address surfaces at Load() time rather than
// at the first Etherscan call.
func TestLoad_BatchAddressValidation_BatcherFromAddress(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[batch]
batcher_from_address = "not-an-address"
batch_inbox_to_address = "0x00B607c67e6662aC51C747961b657659BB47FD95"

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid batcher_from_address")
	}
	if !strings.Contains(err.Error(), "batcher_from_address") {
		t.Errorf("error should mention batcher_from_address: %v", err)
	}
}

// TestLoad_BatchAddressValidation_BatchInboxToAddress mirrors the
// batcher_from_address check for the inbox side.
func TestLoad_BatchAddressValidation_BatchInboxToAddress(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[batch]
batcher_from_address = "0xdf05E8C9C0Ef7b85d2536182fa1E911622622542"
batch_inbox_to_address = "0xnothex"

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid batch_inbox_to_address")
	}
	if !strings.Contains(err.Error(), "batch_inbox_to_address") {
		t.Errorf("error should mention batch_inbox_to_address: %v", err)
	}
}

// TestLoad_BatchAddressValidation_BothValid asserts both addresses in
// canonical form pass Load() without error and the parsed values are
// preserved verbatim (case sensitivity is meaningful for EIP-55
// checksums, so no case-folding here).
func TestLoad_BatchAddressValidation_BothValid(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[batch]
batcher_from_address = "0xdf05E8C9C0Ef7b85d2536182fa1E911622622542"
batch_inbox_to_address = "0x00B607c67e6662aC51C747961b657659BB47FD95"
start_block = 10929965

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.Batch.BatcherFromAddress, "0xdf05E8C9C0Ef7b85d2536182fa1E911622622542"; got != want {
		t.Errorf("BatcherFromAddress: got %q, want %q", got, want)
	}
	if got, want := c.Batch.BatchInboxToAddress, "0x00B607c67e6662aC51C747961b657659BB47FD95"; got != want {
		t.Errorf("BatchInboxToAddress: got %q, want %q", got, want)
	}
	if got, want := c.Batch.StartBlock, uint64(10929965); got != want {
		t.Errorf("StartBlock: got %d, want %d", got, want)
	}
}

// TestLoad_CacheTTLDefault covers the three TTL parse outcomes:
// unset → 10m default; explicit positive → parsed; zero/negative →
// load error. Mirrors the existing duration tests for namespace and
// txpool timeouts.
func TestLoad_CacheTTLDefault(t *testing.T) {
	t.Run("unset_defaults_to_10m", func(t *testing.T) {
		p := writeTemp(t, "config.toml", `
[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
		c, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got, want := c.Batch.CacheTTL, 10*time.Minute; got != want {
			t.Errorf("CacheTTL default: got %v, want %v", got, want)
		}
	})
	t.Run("explicit_5m", func(t *testing.T) {
		p := writeTemp(t, "config.toml", `
[batch]
cache_ttl = "5m"

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
		c, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got, want := c.Batch.CacheTTL, 5*time.Minute; got != want {
			t.Errorf("CacheTTL: got %v, want %v", got, want)
		}
	})
	t.Run("zero_rejected", func(t *testing.T) {
		p := writeTemp(t, "config.toml", `
[batch]
cache_ttl = "0s"

[backends.sequencer]
consensus_rpc_url = "http://a:1"
`)
		_, err := Load(p)
		if err == nil {
			t.Fatal("expected error for zero cache_ttl")
		}
		if !strings.Contains(err.Error(), "cache_ttl must be positive") {
			t.Errorf("error should mention positive requirement: %v", err)
		}
	})
}
