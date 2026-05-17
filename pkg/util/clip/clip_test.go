package clip

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestCopyOSC52Standard(t *testing.T) {
	t.Setenv("TMUX", "")
	var buf bytes.Buffer
	if err := copyOSC52(&buf, "hello"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	want := "\033]52;c;" + base64.StdEncoding.EncodeToString([]byte("hello")) + "\a"
	if out != want {
		t.Errorf("OSC52 = %q, want %q", out, want)
	}
}

func TestCopyOSC52Tmux(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
	var buf bytes.Buffer
	if err := copyOSC52(&buf, "x"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "\033Ptmux;") || !strings.HasSuffix(out, "\033\\") {
		t.Errorf("tmux passthrough wrapper missing: %q", out)
	}
}

func TestCopyOSC52NilWriter(t *testing.T) {
	if err := copyOSC52(nil, "x"); err == nil {
		t.Error("expected error for nil writer")
	}
}

func TestCopyOSC52Truncation(t *testing.T) {
	var buf bytes.Buffer
	big := strings.Repeat("a", 20000)
	if err := copyOSC52(&buf, big); err != nil {
		t.Fatal(err)
	}
	// payload is truncated to 10k bytes before base64 encoding
	payload := strings.TrimSuffix(strings.TrimPrefix(buf.String(), "\033]52;c;"), "\a")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 10000 {
		t.Errorf("decoded len = %d, want 10000", len(decoded))
	}
}
