//go:build integration

// Integration tests for compare-and-swap saves (N-1) and watches (F-1),
// run against both protocols. The v3 endpoint is the usual
// ETCD_WALKER_TEST_ENDPOINT; the v2 one is ETCD_WALKER_TEST_V2_ENDPOINT
// (an etcd serving the v2 API, e.g. 3.3 or 3.4 with --enable-v2), and the v2
// cases are skipped when it is unset.
package model

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type protoCase struct {
	name string
	m    func(t *testing.T) *Model
}

func protocols() []protoCase {
	return []protoCase{
		{"v3", integrationModel},
		{"v2", integrationModelV2},
	}
}

func integrationModelV2(t *testing.T) *Model {
	t.Helper()
	endpoint := os.Getenv("ETCD_WALKER_TEST_V2_ENDPOINT")
	if endpoint == "" {
		t.Skip("ETCD_WALKER_TEST_V2_ENDPOINT not set")
	}
	host, port, ok := strings.Cut(endpoint, ":")
	if !ok {
		t.Fatalf("ETCD_WALKER_TEST_V2_ENDPOINT %q is not host:port", endpoint)
	}
	m, err := NewModel(Options{Host: host, Port: port, Protocol: "v2", TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("connecting to %s: %v", endpoint, err)
	}
	return m
}

func TestIntegrationCASSave(t *testing.T) {
	for _, pc := range protocols() {
		t.Run(pc.name, func(t *testing.T) {
			m := pc.m(t)
			p := scratch(t, m)
			k := p + "/k"
			if err := m.Set(k, "v1"); err != nil {
				t.Fatal(err)
			}
			base, err := m.Get(k)
			if err != nil {
				t.Fatal(err)
			}

			// Unchanged since read: the conditional save lands.
			if err := m.SetKeepTTL(k, "v2", base.LeaseID, base.TTL, base.ModRev); err != nil {
				t.Fatalf("CAS on an unchanged key: %v", err)
			}
			// The same stale base again: someone (us) wrote since, so refuse.
			err = m.SetKeepTTL(k, "stale", base.LeaseID, base.TTL, base.ModRev)
			ce, ok := AsConflict(err)
			if !ok {
				t.Fatalf("stale CAS err = %v, want ConflictError", err)
			}
			now, _ := m.Get(k)
			if now.Value != "v2" {
				t.Errorf("value = %q: the refused save was written anyway", now.Value)
			}
			if ce.Actual != now.ModRev || ce.Deleted {
				t.Errorf("conflict = %+v, want actual %d", ce, now.ModRev)
			}

			// Deleted meanwhile: refuse rather than resurrect.
			if err := m.Del(k); err != nil {
				t.Fatal(err)
			}
			ce, ok = AsConflict(m.SetKeepTTL(k, "ghost", 0, 0, now.ModRev))
			if !ok || !ce.Deleted {
				t.Errorf("CAS on a deleted key = %+v, want Deleted conflict", ce)
			}
			if _, err := m.Get(k); err == nil {
				t.Error("a refused CAS resurrected the deleted key")
			}
		})
	}
}

// A CAS save must keep the key's TTL (lease on v3, native TTL on v2).
func TestIntegrationCASKeepsTTL(t *testing.T) {
	for _, pc := range protocols() {
		t.Run(pc.name, func(t *testing.T) {
			m := pc.m(t)
			p := scratch(t, m)
			k := p + "/ttl"
			if err := m.SetTTL(k, "v", 120); err != nil {
				t.Fatal(err)
			}
			base, _ := m.Get(k)
			if base.TTL <= 0 {
				t.Fatalf("setup: TTL not applied: %+v", base)
			}
			if err := m.SetKeepTTL(k, "edited", base.LeaseID, base.TTL, base.ModRev); err != nil {
				t.Fatal(err)
			}
			after, _ := m.Get(k)
			if after.Value != "edited" || after.TTL <= 0 {
				t.Errorf("after CAS save = %+v, want the value written and the TTL kept", after)
			}
		})
	}
}

func waitEvent(t *testing.T, ch <-chan WatchEvent, d time.Duration) (WatchEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(d):
		return WatchEvent{}, false
	}
}

func TestIntegrationWatch(t *testing.T) {
	for _, pc := range protocols() {
		t.Run(pc.name, func(t *testing.T) {
			m := pc.m(t)
			p := scratch(t, m)
			if err := m.MkDir(p + "/dir"); err != nil {
				t.Fatal(err)
			}
			// A sibling that shares the string prefix must NOT trigger the
			// watch on dir: /dir vs /dirty.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := m.Watch(ctx, p+"/dir")
			time.Sleep(300 * time.Millisecond) // let the watch establish

			if err := m.Set(p+"/dirty", "x"); err != nil {
				t.Fatal(err)
			}
			if ev, ok := waitEvent(t, ch, 700*time.Millisecond); ok {
				t.Errorf("a write to the sibling /dirty fired the /dir watch: %+v", ev)
			}

			for i, write := range []func() error{
				func() error { return m.Set(p+"/dir/a", "1") },         // create
				func() error { return m.Set(p+"/dir/a", "2") },         // update
				func() error { return m.Set(p+"/dir/deep/er/k", "z") }, // nested
				func() error { return m.Del(p + "/dir/a") },            // delete
				func() error { return m.SetTTL(p+"/dir/exp", "e", 1) }, // ttl key
			} {
				if err := write(); err != nil {
					t.Fatal(err)
				}
				ev, ok := waitEvent(t, ch, 3*time.Second)
				if !ok || ev.Err != nil {
					t.Fatalf("change %d: event = %+v ok=%v", i, ev, ok)
				}
			}
			// TTL expiry is a change too (v2 expire / v3 lease revoke).
			deadline := time.Now().Add(8 * time.Second)
			for {
				ev, ok := waitEvent(t, ch, time.Until(deadline))
				if !ok {
					t.Fatal("expiry of a TTL key under the watched dir produced no event")
				}
				if ev.Err == nil {
					if _, err := m.Get(p + "/dir/exp"); err != nil {
						break // expired, and we were told
					}
				}
			}

			cancel()
			deadline = time.Now().Add(3 * time.Second)
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
				case <-time.After(time.Until(deadline)):
					t.Fatal("watch channel not closed after cancel")
				}
			}
		})
	}
}

// Watching the root must work too (prefix "/" on v3, "/" recursive on v2).
func TestIntegrationWatchRoot(t *testing.T) {
	for _, pc := range protocols() {
		t.Run(pc.name, func(t *testing.T) {
			m := pc.m(t)
			p := scratch(t, m)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := m.Watch(ctx, "/")
			time.Sleep(300 * time.Millisecond)
			if err := m.Set(fmt.Sprintf("%s/root-%d", p, time.Now().UnixNano()), "x"); err != nil {
				t.Fatal(err)
			}
			if ev, ok := waitEvent(t, ch, 3*time.Second); !ok || ev.Err != nil {
				t.Fatalf("root watch: %+v ok=%v", ev, ok)
			}
		})
	}
}

func TestIntegrationPing(t *testing.T) {
	for _, pc := range protocols() {
		t.Run(pc.name, func(t *testing.T) {
			if err := pc.m(t).Ping(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
