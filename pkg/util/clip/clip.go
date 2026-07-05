package clip

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/atotto/clipboard"
)

var ErrNoClipboard = errors.New("no clipboard available")

// ErrTooLarge is returned when only the OSC52 fallback is available and the
// text exceeds what a terminal clipboard sequence can safely carry. Failing
// beats silently handing the user a truncated copy.
var ErrTooLarge = errors.New("value too large for terminal (OSC52) clipboard")

// maxOSC52Len bounds the raw payload of an OSC52 sequence. Many terminals cap
// the whole escape sequence around 100KB; 10k raw bytes keeps us safely under
// that after base64 expansion.
const maxOSC52Len = 10000

// Copy tries system clipboard first. If unavailable, it falls back to OSC52 (terminal clipboard).
func Copy(text string) error {
	// First: system clipboard (xclip/xsel/pbcopy/clip.exe/wl-copy)
	if err := clipboard.WriteAll(text); err == nil {
		return nil
	}
	// Fallback: OSC52 (headless-friendly)
	err := copyOSC52(os.Stdout, text)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrTooLarge) {
		return err
	}
	return ErrNoClipboard
}

// copyOSC52 sends an ANSI OSC52 sequence to the terminal.
// Works over SSH in many terminals. For tmux, see notes below.
func copyOSC52(w io.Writer, s string) error {
	if w == nil {
		return fmt.Errorf("nil writer")
	}

	if len(s) > maxOSC52Len {
		return fmt.Errorf("%w: %d bytes exceeds the %d-byte limit", ErrTooLarge, len(s), maxOSC52Len)
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
	return err
}
