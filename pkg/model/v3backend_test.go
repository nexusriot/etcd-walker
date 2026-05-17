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
		"/a/b":     "hello",
		"/dir/x":   "1",
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
		"/x/a":    "1",
		"/x/b":    "2",
		"/x/.dir": "",
		"/x/s/.dir": "",
		"/x/s/c":  "3",
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
