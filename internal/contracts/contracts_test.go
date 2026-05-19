package contracts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
      "DisputeGameFactoryProxy": "0x9b6709999e8fd16cae9e27bd0e7cf4b747097239"
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
}
