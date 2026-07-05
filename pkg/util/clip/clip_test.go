package clip

import (
	"bytes"
	"encoding/base64"
	"errors"
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

func TestCopyOSC52TooLarge(t *testing.T) {
	var buf bytes.Buffer
	big := strings.Repeat("a", 20000)
	err := copyOSC52(&buf, big)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("copyOSC52(20k bytes) err = %v, want ErrTooLarge", err)
	}
	// Nothing may reach the terminal: a partial/truncated copy is worse
	// than a clean failure.
	if buf.Len() != 0 {
		t.Errorf("copyOSC52 wrote %d bytes despite failing", buf.Len())
	}
}

func TestCopyOSC52AtLimit(t *testing.T) {
	t.Setenv("TMUX", "")
	var buf bytes.Buffer
	exact := strings.Repeat("a", maxOSC52Len)
	if err := copyOSC52(&buf, exact); err != nil {
		t.Fatalf("copyOSC52 at the limit should succeed: %v", err)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(buf.String(), "\033]52;c;"), "\a")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != maxOSC52Len {
		t.Errorf("decoded len = %d, want %d", len(decoded), maxOSC52Len)
	}
}
