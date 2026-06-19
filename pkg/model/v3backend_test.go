package model

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// fakeKV is an in-memory clientv3.KV. A Get/Delete is treated as a prefix
// operation when the resolved Op carries a range end (i.e. WithPrefix was
// used), otherwise as an exact key operation.
type fakeKV struct {
	store map[string]string
}

func newFakeKV() *fakeKV { return &fakeKV{store: map[string]string{}} }

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
			resp.Kvs = append(resp.Kvs, &mvccpb.KeyValue{Key: []byte(k), Value: []byte(f.store[k])})
		}
	} else if v, ok := f.store[key]; ok {
		resp.Kvs = append(resp.Kvs, &mvccpb.KeyValue{Key: []byte(key), Value: []byte(v)})
	}
	resp.Count = int64(len(resp.Kvs))
	return resp, nil
}

func (f *fakeKV) Put(_ context.Context, key, val string, _ ...clientv3.OpOption) (*clientv3.PutResponse, error) {
	f.store[key] = val
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
