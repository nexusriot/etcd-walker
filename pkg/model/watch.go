package model

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	clientv2 "github.com/coreos/etcd/client"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ConflictError is what a conditional write (SetKeepTTL with expectModRev)
// returns when the key changed on the server after the edit was based on it.
// Nothing was written. Actual is the key's ModRev now; Deleted means the key
// no longer exists at all.
type ConflictError struct {
	Key      string
	Expected int64
	Actual   int64
	Deleted  bool
}

func (e *ConflictError) Error() string {
	if e.Deleted {
		return fmt.Sprintf("%s was deleted on the server after revision %d", e.Key, e.Expected)
	}
	if e.Actual > 0 {
		return fmt.Sprintf("%s changed on the server (revision %d → %d)", e.Key, e.Expected, e.Actual)
	}
	return fmt.Sprintf("%s changed on the server after revision %d", e.Key, e.Expected)
}

// AsConflict unwraps err into a *ConflictError, if it is one.
func AsConflict(err error) (*ConflictError, bool) {
	var ce *ConflictError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

// WatchEvent is one notification from Model.Watch. A zero event means
// "something under the watched prefix changed — re-read it". Err reports a
// watch failure; the watch keeps retrying after one, so an error is a status
// to show, not the end of the stream.
type WatchEvent struct {
	Err error
}

// watchRetry is how long a failed watch waits before re-establishing itself.
// A variable so tests can shorten it.
var watchRetry = time.Second

// watchCheck is how often a v3 watch verifies that the cluster's revision has
// not gone backwards (see the check in v3Backend.watch). A variable so tests
// can shorten it.
var watchCheck = 5 * time.Second

// Watch reports changes anywhere under dir until ctx is cancelled, at which
// point the channel is closed. It is the engine of the live-refresh mode
// (F-1): the controller re-lists on each event. Events carry no payload on
// purpose — a listing is cheap to re-read and always correct, whereas
// patching it from event deltas would have to reproduce every rule of ls
// (synthetic directories, markers, hidden keys) a second time.
func (m *Model) Watch(ctx context.Context, dir string) <-chan WatchEvent {
	return m.backend.watch(ctx, dir)
}

// Ping is a cheap reachability check — the same minimal request the startup
// probe makes. The live mode runs it periodically so a vanished cluster shows
// in the header instead of the UI quietly displaying a frozen tree.
func (m *Model) Ping() error { return m.backend.probe() }

// sendEvent delivers ev unless ctx ends first. It reports whether the
// consumer is still there.
func sendEvent(ctx context.Context, out chan<- WatchEvent, ev WatchEvent) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// sleepCtx waits d or until ctx ends, reporting whether to carry on.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// storeRev is revision() made safe for a backend without a KV client (unit
// tests that only exercise the watcher).
func (b *v3Backend) storeRev() (int64, error) {
	if b.cli == nil {
		return 0, errors.New("no KV client")
	}
	return b.revision()
}

func (b *v3Backend) watch(ctx context.Context, dir string) <-chan WatchEvent {
	out := make(chan WatchEvent, 1)
	go func() {
		defer close(out)
		if b.watcher == nil {
			sendEvent(ctx, out, WatchEvent{Err: errors.New("watch requires a live etcd client")})
			return
		}
		prefix := withTrail(dir)
		// next is the revision to resume from after a broken watch, so a
		// reconnect replays what was missed instead of skipping it.
		var next int64
		// seen is the highest store revision this watch knows the cluster
		// reached. The periodic check below compares the cluster against it.
		var seen int64
		for ctx.Err() == nil {
			opts := []clientv3.OpOption{clientv3.WithPrefix()}
			if next > 0 {
				opts = append(opts, clientv3.WithRev(next))
			} else if cur, err := b.storeRev(); err == nil {
				seen = cur
			}
			// WithRequireLeader: a member cut off from its cluster cancels the
			// watch instead of silently serving no events forever.
			wctx, cancel := context.WithCancel(clientv3.WithRequireLeader(ctx))
			wch := b.watcher.Watch(wctx, prefix, opts...)
			check := time.NewTicker(watchCheck)
			restart := false
		recv:
			for {
				select {
				case <-ctx.Done():
					check.Stop()
					cancel()
					return
				case <-check.C:
					// A cluster recreated from scratch or restored from an
					// older backup has a LOWER revision than this watch has
					// seen. The client transparently resumes the watch at
					// seen+1 — a revision the new cluster will not reach for
					// a long time — so the watch goes silent while looking
					// perfectly healthy. Found by e2e round 2. Detect it and
					// start over from the cluster's present.
					if cur, err := b.storeRev(); err == nil && seen > 0 && cur < seen {
						next, seen = 0, 0
						restart = true
						if !sendEvent(ctx, out, WatchEvent{}) {
							check.Stop()
							cancel()
							return
						}
						break recv
					}
				case resp, ok := <-wch:
					if !ok {
						break recv
					}
					if resp.CompactRevision > 0 {
						// The resume point was compacted away: events are
						// lost, so start fresh and re-read everything.
						next = 0
						restart = true
						if !sendEvent(ctx, out, WatchEvent{}) {
							check.Stop()
							cancel()
							return
						}
						break recv
					}
					if err := resp.Err(); err != nil {
						if !sendEvent(ctx, out, WatchEvent{Err: err}) {
							check.Stop()
							cancel()
							return
						}
						break recv
					}
					if r := resp.Header.GetRevision(); r > seen {
						seen = r
					}
					if len(resp.Events) == 0 {
						continue // progress notification / created ack
					}
					next = resp.Header.GetRevision() + 1
					if !sendEvent(ctx, out, WatchEvent{}) {
						check.Stop()
						cancel()
						return
					}
				}
			}
			check.Stop()
			cancel()
			if restart {
				continue // a deliberate restart needs no back-off
			}
			if !sleepCtx(ctx, watchRetry) {
				return
			}
		}
	}()
	return out
}

func (b *v2Backend) watch(ctx context.Context, dir string) <-chan WatchEvent {
	out := make(chan WatchEvent, 1)
	go func() {
		defer close(out)
		key := normPath(dir)
		for ctx.Err() == nil {
			w := b.api.Watcher(key, &clientv2.WatcherOptions{Recursive: true})
			if w == nil {
				sendEvent(ctx, out, WatchEvent{Err: errors.New("watch requires a live etcd client")})
				return
			}
			for {
				_, err := w.Next(ctx)
				if ctx.Err() != nil {
					return
				}
				if err == nil {
					if !sendEvent(ctx, out, WatchEvent{}) {
						return
					}
					continue
				}
				var e clientv2.Error
				if errors.As(err, &e) && e.Code == clientv2.ErrorCodeEventIndexCleared {
					// v2 keeps only the last 1000 events; falling behind
					// loses some. Re-read everything and watch from now.
					if !sendEvent(ctx, out, WatchEvent{}) {
						return
					}
				} else if !sendEvent(ctx, out, WatchEvent{Err: err}) {
					return
				}
				break
			}
			if !sleepCtx(ctx, watchRetry) {
				return
			}
		}
	}()
	return out
}

// explainTLS adds the real cause to a failed probe over TLS. gRPC retries a
// failed handshake until the dial deadline and then reports only "context
// deadline exceeded", so an untrusted server certificate or a missing client
// certificate looked exactly like an unreachable host. One direct handshake
// with the same tls.Config recovers the actual reason (found by the hermetic
// e2e TLS scenario).
func (b *v3Backend) explainTLS(err error) error {
	if err == nil || b == nil || b.tlsCfg == nil || b.addr == "" {
		return err
	}
	if herr := tlsHandshakeError(b.addr, b.tlsCfg, b.timeout); herr != nil {
		return fmt.Errorf("%w (TLS: %v)", err, herr)
	}
	return err
}

// tlsHandshakeError dials addr and completes a TLS handshake. With TLS 1.3 a
// server that rejects the client certificate only says so after the client's
// handshake has finished, so it also waits briefly for the server's first
// word: an alert there is the rejection.
func tlsHandshakeError(addr string, cfg *tls.Config, timeout time.Duration) error {
	if timeout <= 0 || timeout > 3*time.Second {
		timeout = 3 * time.Second
	}
	// No ALPN: a server that strictly checks protocols would refuse the probe
	// for a reason that has nothing to do with the user's configuration.
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", addr, cfg.Clone())
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, rerr := conn.Read(make([]byte, 1)); rerr != nil {
		var ne net.Error
		if errors.As(rerr, &ne) && ne.Timeout() {
			return nil // the server is waiting for us: the handshake was fine
		}
		if strings.Contains(rerr.Error(), "tls") {
			return rerr
		}
	}
	return nil
}
