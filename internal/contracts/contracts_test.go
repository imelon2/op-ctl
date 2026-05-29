package contracts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeStateFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write state.json: %v", err)
	}
	return p
}

func TestLoad_Success(t *testing.T) {
	p := writeStateFile(t, `{
  "opChainDeployments": [
    {
      "id": "0x000000000000000000000000000000000000000000000000000000000000a5e8",
      "DisputeGameFactoryProxy": "0x9b6709999e8fd16cae9e27bd0e7cf4b747097239",
      "SystemConfigProxy": "0x586fb5eac03e347a9ab109618296d9aad915a2ee"
    }
  ]
}`)
	a, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := a.DisputeGameFactoryProxy, "0x9b6709999e8fd16cae9e27bd0e7cf4b747097239"; got != want {
		t.Errorf("DisputeGameFactoryProxy: got %q, want %q", got, want)
	}
	if got, want := a.SystemConfigProxy, "0x586fb5eac03e347a9ab109618296d9aad915a2ee"; got != want {
		t.Errorf("SystemConfigProxy: got %q, want %q", got, want)
	}
}

// TestLoad_OmitSystemConfigProxy verifies that the field is optional —
// older state.json files without SystemConfigProxy still load fine; the
// consumer (e.g. `read network-fee`) surfaces the missing-address error.
func TestLoad_OmitSystemConfigProxy(t *testing.T) {
	p := writeStateFile(t, `{
  "opChainDeployments": [
    {
      "id": "0xabc",
      "DisputeGameFactoryProxy": "0x9b6709999e8fd16cae9e27bd0e7cf4b747097239"
    }
  ]
}`)
	a, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a.SystemConfigProxy != "" {
		t.Errorf("absent SystemConfigProxy should leave empty string, got %q", a.SystemConfigProxy)
	}
}

func TestLoad_EmptyPath(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Fatal("expected error for empty path")
	} else if !strings.Contains(err.Error(), "state_root path is empty") {
		t.Errorf("error: %v", err)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/state.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_NoDeployments(t *testing.T) {
	p := writeStateFile(t, `{"opChainDeployments": []}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for empty deployments")
	}
	if !strings.Contains(err.Error(), "no opChainDeployments entries") {
		t.Errorf("error: %v", err)
	}
}

func TestLoad_MissingFactory(t *testing.T) {
	p := writeStateFile(t, `{"opChainDeployments": [{"id": "0xabc"}]}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for missing DisputeGameFactoryProxy")
	}
	if !strings.Contains(err.Error(), "DisputeGameFactoryProxy") {
		t.Errorf("error: %v", err)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	p := writeStateFile(t, `not json`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoad_L2BlockTimeFromGlobalDeployOverrides(t *testing.T) {
	p := writeStateFile(t, `{
  "appliedIntent": {
    "globalDeployOverrides": {
      "l2BlockTime": 3
    }
  },
  "opChainDeployments": [
    {
      "id": "0xabc",
      "DisputeGameFactoryProxy": "0x9b6709999e8fd16cae9e27bd0e7cf4b747097239"
    }
  ]
}`)
	a, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := a.L2BlockTime, 3*time.Second; got != want {
		t.Errorf("L2BlockTime: got %v, want %v", got, want)
	}
}

// Absent globalDeployOverrides must leave L2BlockTime at zero so consumers
// can apply their own fallback (config.toml's l2_block_time or default).
func TestLoad_L2BlockTimeAbsent(t *testing.T) {
	p := writeStateFile(t, `{
  "opChainDeployments": [
    {
      "id": "0xabc",
      "DisputeGameFactoryProxy": "0x9b6709999e8fd16cae9e27bd0e7cf4b747097239"
    }
  ]
}`)
	a, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a.L2BlockTime != 0 {
		t.Errorf("absent L2BlockTime should leave zero, got %v", a.L2BlockTime)
	}
}

// LoadL2BlockTime must not require DisputeGameFactoryProxy — the status-block
// path needs only the cadence and shouldn't break on partially-populated
// state.json files.
func TestLoadL2BlockTime_NoDeployments(t *testing.T) {
	p := writeStateFile(t, `{
  "appliedIntent": {
    "globalDeployOverrides": {
      "l2BlockTime": 2
    }
  }
}`)
	if got, want := LoadL2BlockTime(p), 2*time.Second; got != want {
		t.Errorf("LoadL2BlockTime: got %v, want %v", got, want)
	}
}

// Missing / unreadable / malformed state.json must return 0, not panic,
// so the status-block command stays usable even when the file is absent.
func TestLoadL2BlockTime_Tolerant(t *testing.T) {
	if got := LoadL2BlockTime(""); got != 0 {
		t.Errorf("empty path: got %v, want 0", got)
	}
	if got := LoadL2BlockTime("/nonexistent/state.json"); got != 0 {
		t.Errorf("missing file: got %v, want 0", got)
	}
	bad := writeStateFile(t, `not json`)
	if got := LoadL2BlockTime(bad); got != 0 {
		t.Errorf("malformed json: got %v, want 0", got)
	}
}

func TestLoad_RealStateFile(t *testing.T) {
	// Smoke test against the repo's actual state.json — confirms our
	// loader matches the operator's on-disk format.
	repo, err := filepath.Abs("../../config/state.json")
	if err != nil {
		t.Skipf("resolve repo state.json path: %v", err)
	}
	if _, err := os.Stat(repo); err != nil {
		t.Skipf("no state.json present, skip: %v", err)
	}
	a, err := Load(repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.HasPrefix(a.DisputeGameFactoryProxy, "0x") || len(a.DisputeGameFactoryProxy) != 42 {
		t.Errorf("DisputeGameFactoryProxy does not look like an address: %q", a.DisputeGameFactoryProxy)
	}
	// The repo's state.json sets globalDeployOverrides.l2BlockTime = 1
	// (one second). Asserting on it pins the expectation that op-ctl
	// pulls the cadence from state.json rather than the [global] section
	// of config.toml.
	if got, want := a.L2BlockTime, 1*time.Second; got != want {
		t.Errorf("L2BlockTime: got %v, want %v", got, want)
	}
}

// TestLoadL2ChainID_Present asserts the L2 chain id is extracted from
// the first opChainDeployments entry. `op-ctl read batch` uses this to
// partition its SQLite cache directory.
func TestLoadL2ChainID_Present(t *testing.T) {
	p := writeStateFile(t, `{
  "opChainDeployments": [
    {
      "id": "0xa5e8",
      "DisputeGameFactoryProxy": "0x9b6709999e8fd16cae9e27bd0e7cf4b747097239"
    }
  ]
}`)
	got, err := LoadL2ChainID(p)
	if err != nil {
		t.Fatalf("LoadL2ChainID: %v", err)
	}
	if want := "0xa5e8"; got != want {
		t.Errorf("LoadL2ChainID: got %q, want %q", got, want)
	}
}

// TestLoadL2ChainID_Absent surfaces a clear error when state.json has
// no deployments — better than a silent empty-string return that
// would manifest as a `config//batcher.db` path bug downstream.
func TestLoadL2ChainID_Absent(t *testing.T) {
	p := writeStateFile(t, `{"opChainDeployments": []}`)
	_, err := LoadL2ChainID(p)
	if err == nil {
		t.Fatal("expected error for empty opChainDeployments")
	}
	if !strings.Contains(err.Error(), "no opChainDeployments") {
		t.Errorf("error should mention missing deployments: %v", err)
	}
}

// TestLoadL2ChainID_PreservesCase guards the explicit "do not lowercase"
// rule from the deep-interview spec (L131). EIP-55 / on-disk case is
// meaningful when the value becomes a directory name; folding it would
// silently drift paths between an op-deployer-emitted file and a
// repo-cloned working copy.
func TestLoadL2ChainID_PreservesCase(t *testing.T) {
	p := writeStateFile(t, `{
  "opChainDeployments": [{ "id": "0xAA36A7", "DisputeGameFactoryProxy": "0x9b6709999e8fd16cae9e27bd0e7cf4b747097239" }]
}`)
	got, err := LoadL2ChainID(p)
	if err != nil {
		t.Fatalf("LoadL2ChainID: %v", err)
	}
	if got != "0xAA36A7" {
		t.Errorf("LoadL2ChainID should preserve case: got %q, want %q", got, "0xAA36A7")
	}
}

// TestLoadL2ChainID_PathErrors covers the early-fail branches: empty
// path, missing file, malformed JSON. Symmetric with
// TestLoadL2BlockTime_Tolerant but the helper returns errors rather
// than the silent zero used for duration-fallback semantics.
func TestLoadL2ChainID_PathErrors(t *testing.T) {
	if _, err := LoadL2ChainID(""); err == nil {
		t.Error("empty path should error")
	}
	if _, err := LoadL2ChainID("/nonexistent/state.json"); err == nil {
		t.Error("missing file should error")
	}
	bad := writeStateFile(t, `not json`)
	if _, err := LoadL2ChainID(bad); err == nil {
		t.Error("malformed json should error")
	}
}
