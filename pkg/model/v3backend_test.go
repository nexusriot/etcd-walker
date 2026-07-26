package model

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// fakeKV is an in-memory clientv3.KV. A Get/Delete is treated as a prefix
// operation when the resolved Op carries a range end (i.e. WithPrefix was
// used), otherwise as an exact key operation. Leases attached via WithLease
// are tracked per key so tests can assert lease preservation.
type fakeKV struct {
	store  map[string]string
	leases map[string]int64
	// hist holds optional per-key revision history (oldest → newest) served
	// to exact-key Gets; a Get with WithRev(r) returns the newest entry with
	// rev <= r, the way real MVCC reads behave.
	hist map[string][]fakeRev
	// compactRev makes Gets below this revision fail with ErrCompacted,
	// modelling a compacted cluster.
	compactRev int64
}

type fakeRev struct {
	rev, version int64
	value        string
}

func newFakeKV() *fakeKV {
	return &fakeKV{
		store:  map[string]string{},
		leases: map[string]int64{},
		hist:   map[string][]fakeRev{},
	}
}

// seedHistory installs a revision history for key and mirrors the newest
// value into the flat store so listings and plain gets stay coherent.
func seedHistory(kv *fakeKV, key string, entries ...fakeRev) {
	kv.hist[key] = entries
	kv.store[key] = entries[len(entries)-1].value
}

// kvAt materializes one historical entry; CreateRevision is the
// incarnation's first stored revision.
func (f *fakeKV) kvAt(key string, e fakeRev) *mvccpb.KeyValue {
	return &mvccpb.KeyValue{
		Key:            []byte(key),
		Value:          []byte(e.value),
		Lease:          f.leases[key],
		CreateRevision: f.hist[key][0].rev,
		ModRevision:    e.rev,
		Version:        e.version,
	}
}

// Fixed revision metadata stamped on every KV the fake returns, so tests can
// assert the backend maps CreateRevision/ModRevision/Version through.
const (
	fakeCreateRev = int64(10)
	fakeModRev    = int64(20)
	fakeVersion   = int64(2)
)

func (f *fakeKV) kv(key string) *mvccpb.KeyValue {
	return &mvccpb.KeyValue{
		Key:            []byte(key),
		Value:          []byte(f.store[key]),
		Lease:          f.leases[key],
		CreateRevision: fakeCreateRev,
		ModRevision:    fakeModRev,
		Version:        fakeVersion,
	}
}

// opLease extracts the lease id a Put would carry. clientv3.Op has no
// exported accessor for it, so read the unexported field reflectively —
// kind-specific getters like Int() are allowed on unexported fields.
func opLease(key, val string, opts []clientv3.OpOption) int64 {
	op := clientv3.OpPut(key, val, opts...)
	f := reflect.ValueOf(op).FieldByName("leaseID")
	if !f.IsValid() {
		return 0
	}
	return f.Int()
}

func hdr() *etcdserverpb.ResponseHeader {
	return &etcdserverpb.ResponseHeader{ClusterId: 42}
}

func (f *fakeKV) Get(_ context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	op := clientv3.OpGet(key, opts...)
	resp := &clientv3.GetResponse{Header: hdr()}
	if len(op.RangeBytes()) > 0 { // prefix get
		keys := make([]string, 0, len(f.store))
		for k := range f.store {
			if strings.HasPrefix(k, key) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			resp.Kvs = append(resp.Kvs, f.kv(k))
		}
	} else if entries, ok := f.hist[key]; ok && len(entries) > 0 {
		if rev := op.Rev(); rev > 0 {
			if f.compactRev > 0 && rev < f.compactRev {
				return nil, rpctypes.ErrCompacted
			}
			for i := len(entries) - 1; i >= 0; i-- {
				if entries[i].rev <= rev {
					resp.Kvs = append(resp.Kvs, f.kvAt(key, entries[i]))
					break
				}
			}
		} else {
			resp.Kvs = append(resp.Kvs, f.kvAt(key, entries[len(entries)-1]))
		}
	} else if _, ok := f.store[key]; ok {
		resp.Kvs = append(resp.Kvs, f.kv(key))
	}
	if op.IsKeysOnly() { // like real etcd: metadata stays, values are stripped
		for _, kv := range resp.Kvs {
			kv.Value = nil
		}
	}
	resp.Count = int64(len(resp.Kvs))
	return resp, nil
}

func (f *fakeKV) Put(_ context.Context, key, val string, opts ...clientv3.OpOption) (*clientv3.PutResponse, error) {
	f.store[key] = val
	if l := opLease(key, val, opts); l != 0 {
		f.leases[key] = l
	} else {
		delete(f.leases, key) // a plain Put detaches any lease, like real etcd
	}
	return &clientv3.PutResponse{Header: hdr()}, nil
}

func (f *fakeKV) Delete(_ context.Context, key string, opts ...clientv3.OpOption) (*clientv3.DeleteResponse, error) {
	op := clientv3.OpDelete(key, opts...)
	n := int64(0)
	if len(op.RangeBytes()) > 0 {
		for k := range f.store {
			if strings.HasPrefix(k, key) {
				delete(f.store, k)
				n++
			}
		}
	} else if _, ok := f.store[key]; ok {
		delete(f.store, key)
		n = 1
	}
	return &clientv3.DeleteResponse{Header: hdr(), Deleted: n}, nil
}

func (f *fakeKV) Compact(context.Context, int64, ...clientv3.CompactOption) (*clientv3.CompactResponse, error) {
	return &clientv3.CompactResponse{Header: hdr()}, nil
}
func (f *fakeKV) Do(context.Context, clientv3.Op) (clientv3.OpResponse, error) {
	return clientv3.OpResponse{}, nil
}
func (f *fakeKV) Txn(context.Context) clientv3.Txn { return nil }

func newTestBackend(seed map[string]string) (*v3Backend, *fakeKV) {
	kv := newFakeKV()
	for k, v := range seed {
		kv.store[k] = v
	}
	return &v3Backend{cli: kv, timeout: 2 * time.Second}, kv
}

func TestV3LsSplitsDirsAndFiles(t *testing.T) {
	b, _ := newTestBackend(map[string]string{
		"/app/key1":          "v1",
		"/app/key2":          "v2",
		"/app/sub/inner":     "iv",
		"/app/sub/deep/leaf": "lv",
		"/app/.dir":          "",
	})
	nodes, err := b.ls("/app")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range nodes {
		got[n.Name] = n.IsDir
	}
	if got["/app/key1"] || got["/app/key2"] {
		t.Errorf("key1/key2 should be files: %+v", got)
	}
	if !got["/app/sub"] {
		t.Errorf("/app/sub should be a directory: %+v", got)
	}
	if _, ok := got["/app/.dir"]; ok {
		t.Errorf(".dir marker must not be listed: %+v", got)
	}
	if n := nodes[0].ClusterId; n != "42" {
		t.Errorf("ClusterId = %q, want 42", n)
	}
}

func TestV3GetExactAndDir(t *testing.T) {
	b, _ := newTestBackend(map[string]string{
		"/a/b":      "hello",
		"/dir/x":    "1",
		"/dir/.dir": "",
	})
	n, err := b.get("/a/b")
	if err != nil {
		t.Fatal(err)
	}
	if n.IsDir || n.Value != "hello" {
		t.Errorf("get(/a/b) = %+v, want file value hello", n)
	}
	d, err := b.get("/dir")
	if err != nil {
		t.Fatal(err)
	}
	if !d.IsDir {
		t.Errorf("get(/dir) should be a directory: %+v", d)
	}
	if _, err := b.get("/missing"); err == nil {
		t.Error("get(/missing) should return not-found error")
	}
}

func TestV3ExportSkipsDirMarker(t *testing.T) {
	b, _ := newTestBackend(map[string]string{
		"/x/a":      "1",
		"/x/b":      "2",
		"/x/.dir":   "",
		"/x/s/.dir": "",
		"/x/s/c":    "3",
	})
	out, err := b.export("/x")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"/x/a": "1", "/x/b": "2", "/x/s/c": "3"}
	if len(out) != len(want) {
		t.Fatalf("export = %v, want %v", out, want)
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("export[%q] = %q, want %q", k, out[k], v)
		}
	}
}

func TestV3RenameKey(t *testing.T) {
	b, kv := newTestBackend(map[string]string{"/old/k": "val"})
	if err := b.renameKey("/old/k", "/new/k"); err != nil {
		t.Fatal(err)
	}
	if _, ok := kv.store["/old/k"]; ok {
		t.Error("old key should be gone after rename")
	}
	if kv.store["/new/k"] != "val" {
		t.Errorf("new key = %q, want val", kv.store["/new/k"])
	}
}

func TestV3RenameDirSibling(t *testing.T) {
	b, kv := newTestBackend(map[string]string{
		"/a/x":     "1",
		"/a/sub/y": "2",
	})
	if err := b.renameDir("/a", "/b"); err != nil {
		t.Fatal(err)
	}
	if kv.store["/b/x"] != "1" || kv.store["/b/sub/y"] != "2" {
		t.Errorf("renamed tree not intact: %+v", kv.store)
	}
	for k := range kv.store {
		if strings.HasPrefix(k, "/a/") {
			t.Errorf("source key %q should be deleted", k)
		}
	}
}

func TestModelImportOverwriteAndSkip(t *testing.T) {
	b, kv := newTestBackend(map[string]string{"/cfg/a": "old"})
	m := &Model{backend: b}

	// skip mode: existing /cfg/a kept, new /cfg/b written.
	w, s, err := m.Import(map[string]string{"/cfg/a": "new", "/cfg/b": "2"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if w != 1 || s != 1 {
		t.Errorf("skip mode: written=%d skipped=%d, want 1/1", w, s)
	}
	if kv.store["/cfg/a"] != "old" || kv.store["/cfg/b"] != "2" {
		t.Errorf("skip mode store wrong: %+v", kv.store)
	}

	// overwrite mode: existing /cfg/a replaced.
	w, s, err = m.Import(map[string]string{"/cfg/a": "new"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if w != 1 || s != 0 {
		t.Errorf("overwrite mode: written=%d skipped=%d, want 1/0", w, s)
	}
	if kv.store["/cfg/a"] != "new" {
		t.Errorf("overwrite mode did not replace value: %q", kv.store["/cfg/a"])
	}
}

func TestModelImportNormalizesAndSkipsRoot(t *testing.T) {
	b, kv := newTestBackend(nil)
	m := &Model{backend: b}
	w, s, err := m.Import(map[string]string{
		"/x//y/": "v",
		"/":      "ignored",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if w != 1 || s != 1 {
		t.Errorf("written=%d skipped=%d, want 1/1", w, s)
	}
	if kv.store["/x/y"] != "v" {
		t.Errorf("key not normalized: %+v", kv.store)
	}
}

func TestV3SetKeepWritesValue(t *testing.T) {
	b, kv := newTestBackend(map[string]string{"/k": "old"})
	// leaseID is ignored by fakeKV (it does not model leases), but setKeep must
	// still write the new value whether or not a lease is supplied.
	if err := b.setKeep("/k", "new", 7, 0); err != nil {
		t.Fatal(err)
	}
	if kv.store["/k"] != "new" {
		t.Errorf("setKeep(lease) value = %q, want new", kv.store["/k"])
	}
	if err := b.setKeep("/k", "newer", 0, 0); err != nil {
		t.Fatal(err)
	}
	if kv.store["/k"] != "newer" {
		t.Errorf("setKeep(no lease) value = %q, want newer", kv.store["/k"])
	}
}

func TestModelSetKeepTTL(t *testing.T) {
	b, kv := newTestBackend(map[string]string{"/cfg/a": "1"})
	m := &Model{backend: b}
	if err := m.SetKeepTTL("/cfg/a", "2", 0, 0); err != nil {
		t.Fatal(err)
	}
	if kv.store["/cfg/a"] != "2" {
		t.Errorf("SetKeepTTL value = %q, want 2", kv.store["/cfg/a"])
	}
}

// Clearing a TTL (ttlSeconds <= 0) must just rewrite the value; the b.c guard
// means no lease/revoke RPCs are attempted when only the fake KV is wired up.
func TestV3SetTTLClearWritesValue(t *testing.T) {
	b, kv := newTestBackend(map[string]string{"/k": "old"})
	if err := b.setTTL("/k", "fresh", 0); err != nil {
		t.Fatal(err)
	}
	if kv.store["/k"] != "fresh" {
		t.Errorf("setTTL clear value = %q, want fresh", kv.store["/k"])
	}
}

// Granting a TTL needs a live *clientv3.Client; with only the fake KV wired up
// (b.c == nil) setTTL must report the limitation rather than panic.
func TestV3SetTTLGrantRequiresClient(t *testing.T) {
	b, _ := newTestBackend(map[string]string{"/k": "old"})
	if err := b.setTTL("/k", "v", 60); err == nil {
		t.Error("setTTL with positive TTL and nil client should error")
	}
}

// Renaming a key must carry its lease along so the remaining TTL survives.
func TestV3RenameKeyPreservesLease(t *testing.T) {
	b, kv := newTestBackend(map[string]string{"/old/k": "val"})
	kv.leases["/old/k"] = 777
	if err := b.renameKey("/old/k", "/new/k"); err != nil {
		t.Fatal(err)
	}
	if kv.leases["/new/k"] != 777 {
		t.Errorf("lease not preserved on renameKey: leases=%+v", kv.leases)
	}
}

// Renaming a directory must carry each child's lease; keys without a lease
// must stay lease-free.
func TestV3RenameDirPreservesLeases(t *testing.T) {
	b, kv := newTestBackend(map[string]string{
		"/a/x":     "1",
		"/a/sub/y": "2",
	})
	kv.leases["/a/sub/y"] = 555
	if err := b.renameDir("/a", "/b"); err != nil {
		t.Fatal(err)
	}
	if kv.leases["/b/sub/y"] != 555 {
		t.Errorf("lease not preserved on renameDir: leases=%+v", kv.leases)
	}
	if l, ok := kv.leases["/b/x"]; ok {
		t.Errorf("lease %d invented for un-leased key /b/x", l)
	}
}

// ls is keys-only by contract: values stay on the server and the details
// pane re-fetches the focused key. get keeps returning the value.
func TestV3LsIsKeysOnly(t *testing.T) {
	b, _ := newTestBackend(map[string]string{
		"/app/key1":  "big-value",
		"/app/sub/x": "other",
	})
	nodes, err := b.ls("/app")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Value != "" {
			t.Errorf("ls should not carry values (keys-only), got %q for %s", n.Value, n.Name)
		}
	}
	n, err := b.get("/app/key1")
	if err != nil {
		t.Fatal(err)
	}
	if n.Value != "big-value" {
		t.Errorf("get must still return the value, got %q", n.Value)
	}
}

func TestV3Probe(t *testing.T) {
	b, _ := newTestBackend(map[string]string{"/k": "v"})
	if err := b.probe(); err != nil {
		t.Fatalf("probe on reachable backend: %v", err)
	}
}

// Without a live client (b.c == nil, as in these tests) lease resolution must
// degrade to "no TTL", never panic.
func TestTTLForLeaseNilClient(t *testing.T) {
	b, _ := newTestBackend(nil)
	if got := b.ttlForLease(context.Background(), 5); got != 0 {
		t.Errorf("ttlForLease(nil client) = %d, want 0", got)
	}
	if got := b.ttlForLease(context.Background(), 0); got != 0 {
		t.Errorf("ttlForLease(no lease) = %d, want 0", got)
	}
}

func TestV3GetReturnsRevisions(t *testing.T) {
	b, _ := newTestBackend(map[string]string{"/k": "v"})
	n, err := b.get("/k")
	if err != nil {
		t.Fatal(err)
	}
	if n.CreateRev != fakeCreateRev || n.ModRev != fakeModRev || n.Version != fakeVersion {
		t.Errorf("revisions not mapped through: %+v, want %d/%d/%d",
			n, fakeCreateRev, fakeModRev, fakeVersion)
	}
}

func TestV3SearchByPath(t *testing.T) {
	b, _ := newTestBackend(map[string]string{
		"/app/config/db": "postgres://x",
		"/app/config/ui": "dark",
		"/app/other":     "CONFIG in value",
		"/app/.dir":      "",
	})
	nodes, truncated, err := b.search("/app", "CONFIG", false, 10)
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
	// Case-insensitive path match; value-only match excluded; .dir excluded.
	if !got["/app/config/db"] || !got["/app/config/ui"] {
		t.Errorf("path matches missing: %v", got)
	}
	if got["/app/other"] {
		t.Errorf("value-only match must not appear when inValues=false: %v", got)
	}
}

func TestV3SearchInValues(t *testing.T) {
	b, _ := newTestBackend(map[string]string{
		"/app/a": "the SECRET token",
		"/app/b": "nothing",
	})
	nodes, _, err := b.search("/app", "secret", true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "/app/a" {
		t.Errorf("value search = %+v, want just /app/a", nodes)
	}
}

func TestV3SearchTruncates(t *testing.T) {
	b, _ := newTestBackend(map[string]string{
		"/x/m1": "", "/x/m2": "", "/x/m3": "", "/x/m4": "",
	})
	nodes, truncated, err := b.search("/x", "m", false, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || !truncated {
		t.Errorf("limit 2: got %d nodes, truncated=%t; want 2/true", len(nodes), truncated)
	}
}

func TestV3CopyKey(t *testing.T) {
	b, kv := newTestBackend(map[string]string{"/src/k": "val"})
	kv.leases["/src/k"] = 99
	if err := b.copyKey("/src/k", "/dst/k"); err != nil {
		t.Fatal(err)
	}
	if kv.store["/src/k"] != "val" {
		t.Error("source must be untouched by copy")
	}
	if kv.store["/dst/k"] != "val" {
		t.Errorf("copy value = %q, want val", kv.store["/dst/k"])
	}
	if kv.leases["/dst/k"] != 99 {
		t.Errorf("lease not carried to copy: %+v", kv.leases)
	}
	if err := b.copyKey("/missing", "/dst/m"); err == nil {
		t.Error("copyKey of missing source should error")
	}
	if err := b.copyKey("/src/k", "/src/k"); err == nil {
		t.Error("copyKey onto itself should error")
	}
}

func TestV3CopyDir(t *testing.T) {
	b, kv := newTestBackend(map[string]string{
		"/a/x":     "1",
		"/a/sub/y": "2",
	})
	kv.leases["/a/sub/y"] = 555
	if err := b.copyDir("/a", "/b"); err != nil {
		t.Fatal(err)
	}
	if kv.store["/b/x"] != "1" || kv.store["/b/sub/y"] != "2" {
		t.Errorf("copied tree not intact: %+v", kv.store)
	}
	if kv.store["/a/x"] != "1" || kv.store["/a/sub/y"] != "2" {
		t.Errorf("source tree must be untouched: %+v", kv.store)
	}
	if kv.leases["/b/sub/y"] != 555 {
		t.Errorf("lease not carried to copied key: %+v", kv.leases)
	}
	if err := b.copyDir("/a", "/a/inner"); err == nil {
		t.Error("copyDir into own subtree should be rejected")
	}
	if err := b.copyDir("/a/sub", "/a"); err == nil {
		t.Error("copyDir onto own parent should be rejected")
	}
}

func TestIsReservedName(t *testing.T) {
	if !IsReservedName(".dir") {
		t.Error("IsReservedName(.dir) should be true")
	}
	for _, name := range []string{"dir", ".dirx", "foo", ""} {
		if IsReservedName(name) {
			t.Errorf("IsReservedName(%q) should be false", name)
		}
	}
}

func TestV3HistoryWalksNewestFirst(t *testing.T) {
	b, kv := newTestBackend(nil)
	seedHistory(kv, "/k",
		fakeRev{rev: 5, version: 1, value: "a"},
		fakeRev{rev: 8, version: 2, value: "b"},
		fakeRev{rev: 12, version: 3, value: "c"},
	)
	m := &Model{backend: b}
	revs, truncated, err := m.History("/k", 10)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("full walk must not report truncation")
	}
	wantRevs := []int64{12, 8, 5}
	wantVals := []string{"c", "b", "a"}
	if len(revs) != 3 {
		t.Fatalf("revs = %+v, want 3 entries", revs)
	}
	for i, r := range revs {
		if r.Rev != wantRevs[i] || r.Value != wantVals[i] || r.Version != int64(3-i) {
			t.Errorf("revs[%d] = %+v, want rev=%d version=%d value=%q",
				i, r, wantRevs[i], 3-i, wantVals[i])
		}
	}
}

func TestV3HistoryHonorsLimit(t *testing.T) {
	b, kv := newTestBackend(nil)
	seedHistory(kv, "/k",
		fakeRev{rev: 5, version: 1, value: "a"},
		fakeRev{rev: 8, version: 2, value: "b"},
		fakeRev{rev: 12, version: 3, value: "c"},
	)
	revs, truncated, err := b.history("/k", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 || revs[0].Rev != 12 || revs[1].Rev != 8 {
		t.Fatalf("limited walk = %+v, want revs 12,8", revs)
	}
	if !truncated {
		t.Error("hitting the limit with older versions left must report truncation")
	}
}

// Compaction ends the walk gracefully: the collected newest revisions come
// back without error and without a truncation flag; the caller detects the
// cut from the oldest entry's Version > 1.
func TestV3HistoryStopsAtCompaction(t *testing.T) {
	b, kv := newTestBackend(nil)
	seedHistory(kv, "/k",
		fakeRev{rev: 5, version: 1, value: "a"},
		fakeRev{rev: 8, version: 2, value: "b"},
		fakeRev{rev: 12, version: 3, value: "c"},
	)
	kv.compactRev = 8 // reads below rev 8 fail like a compacted cluster
	revs, truncated, err := b.history("/k", 10)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("compaction cut must not report limit-truncation")
	}
	if len(revs) != 2 || revs[0].Rev != 12 || revs[1].Rev != 8 {
		t.Fatalf("compacted walk = %+v, want revs 12,8", revs)
	}
	if revs[len(revs)-1].Version != 2 {
		t.Errorf("oldest version = %d, want 2 (compaction marker)", revs[len(revs)-1].Version)
	}
}

func TestV3HistorySingleVersion(t *testing.T) {
	b, kv := newTestBackend(nil)
	seedHistory(kv, "/k", fakeRev{rev: 7, version: 1, value: "only"})
	revs, truncated, err := b.history("/k", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 1 || truncated || revs[0].Value != "only" || revs[0].Version != 1 {
		t.Errorf("single-version history = %+v truncated=%t, want one v1 entry", revs, truncated)
	}
}

func TestV3HistoryMissingKey(t *testing.T) {
	b, _ := newTestBackend(nil)
	if _, _, err := b.history("/nope", 10); err == nil {
		t.Error("history of a missing key should error")
	}
}

// Renaming a directory into its own subtree must not silently destroy data.
func TestV3RenameDirIntoOwnSubtree(t *testing.T) {
	b, kv := newTestBackend(map[string]string{
		"/a/x":     "1",
		"/a/sub/y": "2",
	})
	err := b.renameDir("/a", "/a/child")
	if err == nil {
		// If the implementation "succeeds", the original data must still be
		// reachable somewhere; a wipe is the failure we are guarding against.
		if len(kv.store) == 0 {
			t.Fatal("renameDir(/a -> /a/child) wiped the store: data loss")
		}
		t.Fatalf("renameDir into own subtree should be rejected, got success with store=%+v", kv.store)
	}
}
