package clip

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/atotto/clipboard"
)

var ErrNoClipboard = errors.New("no clipboard available")

// Copy tries system clipboard first. If unavailable, it falls back to OSC52 (terminal clipboard).
func Copy(text string) error {
	// First: system clipboard (xclip/xsel/pbcopy/clip.exe/wl-copy)
	if err := clipboard.WriteAll(text); err == nil {
		return nil
	} else {
		// Fallback: OSC52 (headless-friendly)
		if err2 := copyOSC52(os.Stdout, text); err2 == nil {
			return nil
		}
		return ErrNoClipboard
	}
}

// copyOSC52 sends an ANSI OSC52 sequence to the terminal.
// Works over SSH in many terminals. For tmux, see notes below.
func copyOSC52(w io.Writer, s string) error {
	if w == nil {
		return fmt.Errorf("nil writer")
	}

	// Some terminals have length limits; keep it reasonable.
	// Many support more, but 10k is a safe-ish default.
	const maxLen = 10000
	if len(s) > maxLen {
		s = s[:maxLen]
	}

	// OSC52 wants base64 of bytes
	b64 := base64.StdEncoding.EncodeToString([]byte(s))

	// If running inside tmux, wrap sequence so tmux passes it through.
	// tmux needs: set -g allow-passthrough on  (or newer versions handle it with proper wrapping)
	if os.Getenv("TMUX") != "" {
		// Wrap for tmux: ESC P tmux; ESC <osc52> BEL ESC \
		seq := "\033Ptmux;\033\033]52;c;" + b64 + "\a\033\\"
		_, err := io.WriteString(w, seq)
		return err
	}

	// Standard OSC52: ESC ] 52 ; c ; <b64> BEL
	seq := "\033]52;c;" + b64 + "\a"
	_, err := io.WriteString(w, seq)
	// Some terminals prefer ST instead of BEL; if you want belt+suspenders:
	// _, err = io.WriteString(w, "\033]52;c;"+b64+"\a\033\\")
	_ = strings.Builder{} // no-op to keep imports stable if you tweak above
	return err
}
