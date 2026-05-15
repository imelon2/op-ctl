package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverConfigs_Empty(t *testing.T) {
	dir := t.TempDir()
	got, err := DiscoverConfigs(dir)
	if err != nil {
		t.Fatalf("DiscoverConfigs(empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty dir should return [], got %v", got)
	}
}

func TestDiscoverConfigs_Missing(t *testing.T) {
	// Build a path that we are confident does not exist by appending a
	// suffix to a TempDir.
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	got, err := DiscoverConfigs(missing)
	if err != nil {
		t.Fatalf("DiscoverConfigs(missing): want nil error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing dir should return [], got %v", got)
	}
}

func TestDiscoverConfigs_MultipleSorted(t *testing.T) {
	dir := t.TempDir()
	// Create out of order so the sort step is exercised.
	for _, name := range []string{"config.pp-testnet.toml", "config.mainnet.toml", "readme.md", "config.pp-local.toml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(""), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got, err := DiscoverConfigs(dir)
	if err != nil {
		t.Fatalf("DiscoverConfigs: %v", err)
	}
	want := []string{
		"config.mainnet.toml",
		"config.pp-local.toml",
		"config.pp-testnet.toml",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	gotBases := make([]string, len(got))
	for i, p := range got {
		gotBases[i] = filepath.Base(p)
		if !filepath.IsAbs(p) {
			t.Errorf("entry %d should be absolute path, got %q", i, p)
		}
	}
	if !reflect.DeepEqual(gotBases, want) {
		t.Errorf("base-name order: got %v, want %v", gotBases, want)
	}
}

func TestDiscoverConfigs_HiddenSkipped(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".hidden.toml", "config.real.toml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(""), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got, err := DiscoverConfigs(dir)
	if err != nil {
		t.Fatalf("DiscoverConfigs: %v", err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "config.real.toml" {
		t.Errorf("hidden file should be filtered; got %v", got)
	}
}

func TestDiscoverConfigs_SubdirsIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "inner.toml"), []byte(""), 0o644); err != nil {
		t.Fatalf("write inner: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top.toml"), []byte(""), 0o644); err != nil {
		t.Fatalf("write top: %v", err)
	}
	got, err := DiscoverConfigs(dir)
	if err != nil {
		t.Fatalf("DiscoverConfigs: %v", err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "top.toml" {
		t.Errorf("nested .toml should NOT be returned; got %v", got)
	}
}
