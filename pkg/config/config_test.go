package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingReturnsNilNil(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if cfg != nil {
		t.Fatalf("missing file should return nil config, got %+v", cfg)
	}
}

func TestLoadValid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"host":"h","port":"2379","protocol":"v3","timeout_seconds":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil || cfg.Host != "h" || cfg.Port != "2379" || cfg.Protocol != "v3" || cfg.TimeoutSeconds != 7 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestLoadDirectoryIsError(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("expected error when path is a directory")
	}
}

// The safety settings must survive a round trip through JSON, since a
// mistyped key silently leaving read_only false is the failure that matters.
func TestLoadSafetySettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	body := `{"host":"h","read_only":true,"protected_prefixes":["/registry","/vault"]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ReadOnly {
		t.Error("read_only not parsed")
	}
	if len(cfg.ProtectedPrefixes) != 2 || cfg.ProtectedPrefixes[0] != "/registry" {
		t.Errorf("protected_prefixes = %v", cfg.ProtectedPrefixes)
	}
}

// A config that says nothing about safety leaves the session unrestricted.
func TestLoadSafetyDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"host":"h"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReadOnly || len(cfg.ProtectedPrefixes) != 0 {
		t.Errorf("unexpected safety defaults: %+v", cfg)
	}
}
