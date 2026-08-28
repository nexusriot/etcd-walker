package model

import (
	"context"
	"strings"
	"testing"
	"time"

	clientv2 "github.com/coreos/etcd/client"
)

// fakeKeysAPI is a minimal in-memory clientv2.KeysAPI: Get serves canned
// responses, Set/Delete record their calls so tests can assert what the
// backend wrote (in particular the SetOptions carrying TTLs).
type recordedSet struct {
	key, value string
	opts       *clientv2.SetOptions
}

type fakeKeysAPI struct {
	gets    map[string]*clientv2.Response
	sets    []recordedSet
	deletes []string
}

func (f *fakeKeysAPI) Get(_ context.Context, key string, _ *clientv2.GetOptions) (*clientv2.Response, error) {
	if r, ok := f.gets[key]; ok {
		return r, nil
	}
	return nil, clientv2.Error{Code: clientv2.ErrorCodeKeyNotFound, Message: "not found: " + key}
}

func (f *fakeKeysAPI) Set(_ context.Context, key, value string, opts *clientv2.SetOptions) (*clientv2.Response, error) {
	f.sets = append(f.sets, recordedSet{key: key, value: value, opts: opts})
	return &clientv2.Response{}, nil
}

func (f *fakeKeysAPI) Delete(_ context.Context, key string, _ *clientv2.DeleteOptions) (*clientv2.Response, error) {
	f.deletes = append(f.deletes, key)
	return &clientv2.Response{}, nil
}

func (f *fakeKeysAPI) Create(context.Context, string, string) (*clientv2.Response, error) {
	return &clientv2.Response{}, nil
}

func (f *fakeKeysAPI) CreateInOrder(context.Context, string, string, *clientv2.CreateInOrderOptions) (*clientv2.Response, error) {
	return &clientv2.Response{}, nil
}

func (f *fakeKeysAPI) Update(context.Context, string, string) (*clientv2.Response, error) {
	return &clientv2.Response{}, nil
}

func (f *fakeKeysAPI) Watcher(string, *clientv2.WatcherOptions) clientv2.Watcher { return nil }

func (f *fakeKeysAPI) setFor(key string) *recordedSet {
	for i := range f.sets {
		if f.sets[i].key == key {
			return &f.sets[i]
		}
	}
	return nil
}

// A missing directory lists as empty, not as an error — the browser shows
// an empty level instead of a modal.
func TestV2LsKeyNotFound(t *testing.T) {
	b := &v2Backend{api: &fakeKeysAPI{}, timeout: time.Second}
	nodes, err := b.ls("/missing", 0)
	if err != nil {
		t.Fatalf("ls of missing dir should not error: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("ls of missing dir = %+v, want empty", nodes)
	}
}

func TestV2GetReturnsIndexes(t *testing.T) {
	fake := &fakeKeysAPI{gets: map[string]*clientv2.Response{
		"/k": {Node: &clientv2.Node{Key: "/k", Value: "v", CreatedIndex: 7, ModifiedIndex: 9}},
	}}
	b := &v2Backend{api: fake, timeout: time.Second}
	n, err := b.get("/k", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n.CreateRev != 7 || n.ModRev != 9 || n.Version != 0 {
		t.Errorf("indexes not mapped: %+v, want 7/9/0", n)
	}
}

func TestV2RenameKeyPreservesTTL(t *testing.T) {
	fake := &fakeKeysAPI{gets: map[string]*clientv2.Response{
		"/old": {Node: &clientv2.Node{Key: "/old", Value: "v", TTL: 30}},
	}}
	b := &v2Backend{api: fake, timeout: time.Second}

	if err := b.renameKey("/old", "/new"); err != nil {
		t.Fatal(err)
	}
	s := fake.setFor("/new")
	if s == nil {
		t.Fatalf("no Set recorded for /new: %+v", fake.sets)
	}
	if s.value != "v" {
		t.Errorf("value = %q, want v", s.value)
	}
	if s.opts == nil || s.opts.TTL != 30*time.Second {
		t.Errorf("TTL not preserved on renameKey: opts=%+v", s.opts)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "/old" {
		t.Errorf("old key not deleted: %+v", fake.deletes)
	}
}

func TestV2RenameKeyWithoutTTLStaysPermanent(t *testing.T) {
	fake := &fakeKeysAPI{gets: map[string]*clientv2.Response{
		"/old": {Node: &clientv2.Node{Key: "/old", Value: "v"}},
	}}
	b := &v2Backend{api: fake, timeout: time.Second}

	if err := b.renameKey("/old", "/new"); err != nil {
		t.Fatal(err)
	}
	s := fake.setFor("/new")
	if s == nil {
		t.Fatalf("no Set recorded for /new: %+v", fake.sets)
	}
	if s.opts != nil && s.opts.TTL != 0 {
		t.Errorf("TTL invented for permanent key: opts=%+v", s.opts)
	}
}

func TestV2CopyKeyPreservesTTLAndSource(t *testing.T) {
	fake := &fakeKeysAPI{gets: map[string]*clientv2.Response{
		"/src": {Node: &clientv2.Node{Key: "/src", Value: "v", TTL: 45}},
	}}
	b := &v2Backend{api: fake, timeout: time.Second}

	if err := b.copyKey("/src", "/dst"); err != nil {
		t.Fatal(err)
	}
	s := fake.setFor("/dst")
	if s == nil {
		t.Fatalf("no Set recorded for /dst: %+v", fake.sets)
	}
	if s.value != "v" || s.opts == nil || s.opts.TTL != 45*time.Second {
		t.Errorf("copy lost value or TTL: %+v", s)
	}
	if len(fake.deletes) != 0 {
		t.Errorf("copy must not delete anything: %+v", fake.deletes)
	}
	if err := b.copyKey("/src", "/src"); err == nil {
		t.Error("copyKey onto itself should error")
	}
}

func TestV2CopyDirCopiesTreeWithoutDelete(t *testing.T) {
	fake := &fakeKeysAPI{gets: map[string]*clientv2.Response{
		"/a": {Node: &clientv2.Node{Key: "/a", Dir: true, Nodes: clientv2.Nodes{
			{Key: "/a/k", Value: "1", TTL: 60},
			{Key: "/a/sub", Dir: true, Nodes: clientv2.Nodes{
				{Key: "/a/sub/q", Value: "3"},
			}},
		}}},
	}}
	b := &v2Backend{api: fake, timeout: time.Second}

	if err := b.copyDir("/a", "/b"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"/b", "/b/k", "/b/sub", "/b/sub/q"} {
		if fake.setFor(key) == nil {
			t.Errorf("no Set recorded for %s", key)
		}
	}
	if s := fake.setFor("/b/k"); s != nil && (s.opts == nil || s.opts.TTL != 60*time.Second) {
		t.Errorf("child TTL lost in copy: %+v", s)
	}
	if len(fake.deletes) != 0 {
		t.Errorf("copy must not delete the source: %+v", fake.deletes)
	}
	if err := b.copyDir("/a", "/a/inner"); err == nil {
		t.Error("copyDir into own subtree should be rejected")
	}
}

func TestV2Search(t *testing.T) {
	fake := &fakeKeysAPI{gets: map[string]*clientv2.Response{
		"/": {Node: &clientv2.Node{Key: "/", Dir: true, Nodes: clientv2.Nodes{
			{Key: "/config-db", Value: "postgres"},
			{Key: "/other", Value: "has CONFIG inside"},
			{Key: "/sub", Dir: true, Nodes: clientv2.Nodes{
				{Key: "/sub/config-ui", Value: "dark"},
			}},
		}}},
	}}
	b := &v2Backend{api: fake, timeout: time.Second}

	// Path search, case-insensitive; value-only matches excluded.
	nodes, truncated, err := b.search("/", "CONFIG", false, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("unexpected truncation")
	}
	got := map[string]bool{}
	for _, n := range nodes {
		got[n.Name] = true
	}
	if !got["/config-db"] || !got["/sub/config-ui"] || got["/other"] {
		t.Errorf("path search = %v", got)
	}

	// Value search picks up /other too.
	nodes, _, err = b.search("/", "config", true, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Errorf("value search found %d, want 3: %+v", len(nodes), nodes)
	}

	// Truncation honors the limit.
	nodes, truncated, err = b.search("/", "config", true, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || !truncated {
		t.Errorf("limit 1: got %d/truncated=%t, want 1/true", len(nodes), truncated)
	}
}

func TestV2RenameDirPreservesTTLs(t *testing.T) {
	fake := &fakeKeysAPI{gets: map[string]*clientv2.Response{
		"/old": {Node: &clientv2.Node{Key: "/old", Dir: true, Nodes: clientv2.Nodes{
			{Key: "/old/k", Value: "1", TTL: 60},
			{Key: "/old/p", Value: "2"},
			{Key: "/old/sub", Dir: true, TTL: 120, Nodes: clientv2.Nodes{
				{Key: "/old/sub/q", Value: "3", TTL: 5},
			}},
		}}},
	}}
	b := &v2Backend{api: fake, timeout: time.Second}

	if err := b.renameDir("/old", "/new"); err != nil {
		t.Fatal(err)
	}

	wantTTL := map[string]time.Duration{
		"/new/k":     60 * time.Second,
		"/new/sub":   120 * time.Second,
		"/new/sub/q": 5 * time.Second,
	}
	for key, ttl := range wantTTL {
		s := fake.setFor(key)
		if s == nil {
			t.Errorf("no Set recorded for %s", key)
			continue
		}
		if s.opts == nil || s.opts.TTL != ttl {
			t.Errorf("TTL not preserved for %s: opts=%+v, want %v", key, s.opts, ttl)
		}
	}
	// A key without TTL must not gain one.
	if s := fake.setFor("/new/p"); s == nil {
		t.Error("no Set recorded for /new/p")
	} else if s.opts != nil && s.opts.TTL != 0 {
		t.Errorf("TTL invented for /new/p: opts=%+v", s.opts)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "/old" {
		t.Errorf("source dir not deleted: %+v", fake.deletes)
	}
}

// v2 keeps no previous key values, so history must fail with a descriptive
// error instead of pretending an empty history exists.
func TestV2HistoryUnsupported(t *testing.T) {
	b := &v2Backend{}
	_, _, err := b.history("/k", 10)
	if err == nil || !strings.Contains(err.Error(), "v3") {
		t.Errorf("history on v2 = %v, want v3-required error", err)
	}
}

// The v2 mutators had no coverage at all: v2 expresses expiry as a native
// per-key TTL, so these SetOptions are the whole feature on that protocol.
func TestV2SetWritesPlainValue(t *testing.T) {
	api := &fakeKeysAPI{gets: map[string]*clientv2.Response{}}
	b := &v2Backend{api: api, timeout: time.Second}

	if err := b.set("/k", "v"); err != nil {
		t.Fatal(err)
	}
	if len(api.sets) != 1 {
		t.Fatalf("sets = %+v, want one", api.sets)
	}
	got := api.sets[0]
	if got.key != "/k" || got.value != "v" {
		t.Errorf("set wrote %q=%q", got.key, got.value)
	}
	if got.opts != nil && got.opts.TTL != 0 {
		t.Errorf("plain set attached a TTL: %v", got.opts.TTL)
	}
}

func TestV2SetTTLAppliesAndClears(t *testing.T) {
	api := &fakeKeysAPI{gets: map[string]*clientv2.Response{}}
	b := &v2Backend{api: api, timeout: time.Second}

	if err := b.setTTL("/k", "v", 90); err != nil {
		t.Fatal(err)
	}
	if got := api.sets[0].opts.TTL; got != 90*time.Second {
		t.Errorf("setTTL(90) wrote TTL %v, want 90s", got)
	}

	// Zero must clear the expiry, not leave the old one running.
	if err := b.setTTL("/k", "v", 0); err != nil {
		t.Fatal(err)
	}
	if got := api.sets[1].opts.TTL; got != 0 {
		t.Errorf("setTTL(0) wrote TTL %v, want 0 (permanent)", got)
	}
}

// setKeep is what the value editor uses; on v2 it must re-apply the remaining
// TTL so editing a value does not silently make the key permanent.
func TestV2SetKeepReappliesTTL(t *testing.T) {
	api := &fakeKeysAPI{gets: map[string]*clientv2.Response{}}
	b := &v2Backend{api: api, timeout: time.Second}

	if err := b.setKeep("/k", "new", 0, 45); err != nil {
		t.Fatal(err)
	}
	if got := api.sets[0].opts.TTL; got != 45*time.Second {
		t.Errorf("setKeep kept TTL %v, want 45s", got)
	}
	if err := b.setKeep("/k", "new", 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := api.sets[1].opts.TTL; got != 0 {
		t.Errorf("setKeep on a permanent key wrote TTL %v, want 0", got)
	}
}

func TestV2MkdirDelDeldir(t *testing.T) {
	api := &fakeKeysAPI{gets: map[string]*clientv2.Response{}}
	b := &v2Backend{api: api, timeout: time.Second}

	if err := b.mkdir("/d"); err != nil {
		t.Fatal(err)
	}
	if len(api.sets) != 1 || api.sets[0].opts == nil || !api.sets[0].opts.Dir {
		t.Errorf("mkdir did not request a directory: %+v", api.sets)
	}
	if err := b.del("/k"); err != nil {
		t.Fatal(err)
	}
	if err := b.deldir("/d"); err != nil {
		t.Fatal(err)
	}
	if len(api.deletes) != 2 || api.deletes[0] != "/k" || api.deletes[1] != "/d" {
		t.Errorf("deletes = %v, want [/k /d]", api.deletes)
	}
}

func TestV2Proto(t *testing.T) {
	if got := (&v2Backend{}).proto(); got != "v2" {
		t.Errorf("proto = %q, want v2", got)
	}
}

// v2 keeps no past revisions. Every read must refuse a historical one rather
// than serving current data under a historical label, and revision() has
// nothing to report at all.
func TestV2RefusesRevisionReads(t *testing.T) {
	b := &v2Backend{api: &fakeKeysAPI{}, timeout: time.Second}

	if _, err := b.ls("/", 5); err == nil {
		t.Error("ls at a revision should fail on v2")
	}
	if _, err := b.get("/k", 5); err == nil {
		t.Error("get at a revision should fail on v2")
	}
	if _, err := b.export("/", 5); err == nil {
		t.Error("export at a revision should fail on v2")
	}
	if _, _, err := b.search("/", "q", false, 10, 5); err == nil {
		t.Error("search at a revision should fail on v2")
	}
	if _, err := b.revision(); err == nil {
		t.Error("revision() should fail on v2")
	}
}
