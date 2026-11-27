package model

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	clientv2 "github.com/coreos/etcd/client"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ---- v3 compare-and-swap -------------------------------------------------

// casKV is a fakeKV whose Txn evaluates ModRevision compares, which is all
// setKeep's CAS needs. Every key's ModRevision is modRev[key] (0 = absent).
type casKV struct {
	*fakeKV
	modRev map[string]int64
	txns   int
}

func (f *casKV) Txn(context.Context) clientv3.Txn { f.txns++; return &fakeTxn{kv: f} }

type fakeTxn struct {
	kv               *casKV
	cmps             []clientv3.Cmp
	thenOps, elseOps []clientv3.Op
}

func (t *fakeTxn) If(cs ...clientv3.Cmp) clientv3.Txn { t.cmps = append(t.cmps, cs...); return t }
func (t *fakeTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	t.thenOps = append(t.thenOps, ops...)
	return t
}
func (t *fakeTxn) Else(ops ...clientv3.Op) clientv3.Txn {
	t.elseOps = append(t.elseOps, ops...)
	return t
}

func (t *fakeTxn) Commit() (*clientv3.TxnResponse, error) {
	ok := true
	for _, c := range t.cmps {
		want := c.TargetUnion.(*etcdserverpb.Compare_ModRevision).ModRevision
		if t.kv.modRev[string(c.Key)] != want {
			ok = false
		}
	}
	resp := &clientv3.TxnResponse{Header: hdr(), Succeeded: ok}
	ops := t.elseOps
	if ok {
		ops = t.thenOps
	}
	for _, op := range ops {
		k := string(op.KeyBytes())
		switch {
		case op.IsPut():
			t.kv.store[k] = string(op.ValueBytes())
			if l := reflect.ValueOf(op).FieldByName("leaseID").Int(); l != 0 {
				t.kv.leases[k] = l
			} else {
				delete(t.kv.leases, k)
			}
			t.kv.modRev[k]++
		case op.IsGet():
			rr := &etcdserverpb.RangeResponse{}
			if _, live := t.kv.store[k]; live {
				rr.Kvs = []*mvccpb.KeyValue{{Key: []byte(k), Value: []byte(t.kv.store[k]), ModRevision: t.kv.modRev[k]}}
			}
			resp.Responses = append(resp.Responses, &etcdserverpb.ResponseOp{
				Response: &etcdserverpb.ResponseOp_ResponseRange{ResponseRange: rr}})
		}
	}
	return resp, nil
}

func newCASBackend() (*v3Backend, *casKV) {
	kv := &casKV{fakeKV: newFakeKV(), modRev: map[string]int64{}}
	kv.store["/k"] = "old"
	kv.modRev["/k"] = 20
	return &v3Backend{cli: kv, timeout: time.Second}, kv
}

func TestV3SetKeepCASLandsWhenUnchanged(t *testing.T) {
	b, kv := newCASBackend()
	if err := b.setKeep("/k", "new", 9, 0, 20); err != nil {
		t.Fatalf("setKeep: %v", err)
	}
	if kv.store["/k"] != "new" || kv.leases["/k"] != 9 {
		t.Errorf("value=%q lease=%d, want new/9 (lease must ride along in the txn)", kv.store["/k"], kv.leases["/k"])
	}
}

// The whole point of N-1: someone else wrote the key after the editor read it,
// so the save must not land, and the error must say what the key is now.
func TestV3SetKeepCASRefusesConcurrentWrite(t *testing.T) {
	b, kv := newCASBackend()
	kv.store["/k"] = "theirs"
	kv.modRev["/k"] = 25
	err := b.setKeep("/k", "mine", 0, 0, 20)
	ce, ok := AsConflict(err)
	if !ok {
		t.Fatalf("err = %v, want a ConflictError", err)
	}
	if ce.Expected != 20 || ce.Actual != 25 || ce.Deleted {
		t.Errorf("conflict = %+v, want expected 20, actual 25", ce)
	}
	if kv.store["/k"] != "theirs" {
		t.Errorf("value = %q: the conflicting save overwrote the other writer", kv.store["/k"])
	}
}

// A key deleted while being edited must not be silently resurrected.
func TestV3SetKeepCASRefusesDeletedKey(t *testing.T) {
	b, kv := newCASBackend()
	delete(kv.store, "/k")
	kv.modRev["/k"] = 0
	ce, ok := AsConflict(b.setKeep("/k", "mine", 0, 0, 20))
	if !ok || !ce.Deleted {
		t.Fatalf("conflict = %+v, want Deleted", ce)
	}
	if _, exists := kv.store["/k"]; exists {
		t.Error("CAS save resurrected a deleted key")
	}
}

// expectModRev 0 is the unconditional path; it must not need a Txn at all.
func TestV3SetKeepUnconditionalSkipsTxn(t *testing.T) {
	b, kv := newCASBackend()
	if err := b.setKeep("/k", "blind", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if kv.txns != 0 || kv.store["/k"] != "blind" {
		t.Errorf("txns=%d value=%q, want a plain Put", kv.txns, kv.store["/k"])
	}
}

// ---- v2 compare-and-swap -------------------------------------------------

type casKeysAPI struct {
	*fakeKeysAPI
	setErr  error
	watcher clientv2.Watcher
}

func (f *casKeysAPI) Set(ctx context.Context, key, value string, opts *clientv2.SetOptions) (*clientv2.Response, error) {
	f.sets = append(f.sets, recordedSet{key: key, value: value, opts: opts})
	if f.setErr != nil {
		return nil, f.setErr
	}
	return &clientv2.Response{}, nil
}

func (f *casKeysAPI) Watcher(string, *clientv2.WatcherOptions) clientv2.Watcher { return f.watcher }

func TestV2SetKeepCASSendsPrevIndex(t *testing.T) {
	api := &casKeysAPI{fakeKeysAPI: &fakeKeysAPI{}}
	b := &v2Backend{api: api, timeout: time.Second}
	if err := b.setKeep("/k", "v", 0, 30, 17); err != nil {
		t.Fatal(err)
	}
	o := api.sets[0].opts
	if o.PrevIndex != 17 || o.TTL != 30*time.Second {
		t.Errorf("opts = %+v, want PrevIndex 17 and the TTL kept", o)
	}
}

func TestV2SetKeepCASMapsServerRefusals(t *testing.T) {
	api := &casKeysAPI{fakeKeysAPI: &fakeKeysAPI{gets: map[string]*clientv2.Response{
		"/k": {Node: &clientv2.Node{Key: "/k", ModifiedIndex: 30}},
	}}}
	b := &v2Backend{api: api, timeout: time.Second}

	api.setErr = clientv2.Error{Code: clientv2.ErrorCodeTestFailed, Message: "Compare failed"}
	ce, ok := AsConflict(b.setKeep("/k", "v", 0, 0, 17))
	if !ok || ce.Actual != 30 || ce.Deleted {
		t.Errorf("test-failed → %+v, want conflict with actual 30", ce)
	}

	api.setErr = clientv2.Error{Code: clientv2.ErrorCodeKeyNotFound, Message: "Key not found"}
	ce, ok = AsConflict(b.setKeep("/k", "v", 0, 0, 17))
	if !ok || !ce.Deleted {
		t.Errorf("key-not-found → %+v, want Deleted conflict", ce)
	}

	// Anything else is a plain failure, not a conflict to resolve.
	api.setErr = errors.New("boom")
	if _, ok := AsConflict(b.setKeep("/k", "v", 0, 0, 17)); ok {
		t.Error("an unrelated error was reported as a conflict")
	}
}

func TestConflictErrorText(t *testing.T) {
	cases := map[string]*ConflictError{
		"/k changed on the server (revision 3 → 5)":     {Key: "/k", Expected: 3, Actual: 5},
		"/k was deleted on the server after revision 3": {Key: "/k", Expected: 3, Deleted: true},
		"/k changed on the server after revision 3":     {Key: "/k", Expected: 3},
	}
	for want, e := range cases {
		if e.Error() != want {
			t.Errorf("Error() = %q, want %q", e.Error(), want)
		}
	}
}

// ---- watch ---------------------------------------------------------------

// fakeWatcher hands out channels the test feeds by hand.
type fakeWatcher struct {
	chans    chan clientv3.WatchChan
	prefixes chan string
	revs     chan int64
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{chans: make(chan clientv3.WatchChan, 4), prefixes: make(chan string, 4), revs: make(chan int64, 4)}
}

// Watch behaves like the real client in the way that matters: the returned
// channel closes when ctx ends, or when the test closes its feed.
func (w *fakeWatcher) Watch(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan {
	out := make(chan clientv3.WatchResponse)
	select {
	case w.prefixes <- key:
	case <-ctx.Done():
	}
	select {
	case w.revs <- clientv3.OpGet(key, opts...).Rev():
	case <-ctx.Done():
	}
	var feed clientv3.WatchChan
	select {
	case feed = <-w.chans:
	case <-ctx.Done():
		close(out)
		return out
	}
	go func() {
		defer close(out)
		for {
			select {
			case r, ok := <-feed:
				if !ok {
					return
				}
				select {
				case out <- r:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
func (w *fakeWatcher) RequestProgress(context.Context) error { return nil }
func (w *fakeWatcher) Close() error                          { return nil }

func recv(t *testing.T, ch <-chan WatchEvent) WatchEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed early")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no watch event")
	}
	return WatchEvent{}
}

func TestV3WatchNotifiesAndResumesAfterBreak(t *testing.T) {
	old := watchRetry
	watchRetry = time.Millisecond
	defer func() { watchRetry = old }()

	w := newFakeWatcher()
	b := &v3Backend{watcher: w, timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := make(chan clientv3.WatchResponse, 2)
	w.chans <- first
	out := b.watch(ctx, "/app")

	if p := <-w.prefixes; p != "/app/" {
		t.Errorf("watched %q, want the directory prefix /app/ (not siblings like /apple)", p)
	}
	if r := <-w.revs; r != 0 {
		t.Errorf("first watch starts at rev %d, want now (0)", r)
	}
	first <- clientv3.WatchResponse{Header: etcdserverpb.ResponseHeader{Revision: 41},
		Events: []*clientv3.Event{{Type: mvccpb.PUT, Kv: &mvccpb.KeyValue{Key: []byte("/app/x")}}}}
	if ev := recv(t, out); ev.Err != nil {
		t.Fatalf("event err = %v", ev.Err)
	}

	// The stream breaks (server gone, leader lost): the watch must re-establish
	// itself from the revision after the last one seen, so nothing is skipped.
	second := make(chan clientv3.WatchResponse)
	w.chans <- second
	close(first)
	<-w.prefixes
	if r := <-w.revs; r != 42 {
		t.Errorf("resumed at rev %d, want 42", r)
	}
	cancel()
	for range out {
	}
}

func TestV3WatchCompactionAsksForFullRefresh(t *testing.T) {
	old := watchRetry
	watchRetry = time.Millisecond
	defer func() { watchRetry = old }()

	w := newFakeWatcher()
	b := &v3Backend{watcher: w, timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan clientv3.WatchResponse, 1)
	w.chans <- first
	out := b.watch(ctx, "/")
	<-w.prefixes
	<-w.revs
	first <- clientv3.WatchResponse{CompactRevision: 10, Canceled: true}
	if ev := recv(t, out); ev.Err != nil {
		t.Errorf("compaction should be a refresh, not an error: %v", ev.Err)
	}
	w.chans <- make(chan clientv3.WatchResponse)
	<-w.prefixes
	if r := <-w.revs; r != 0 {
		t.Errorf("after compaction resumed at %d, want now (0)", r)
	}
	cancel()
	for range out {
	}
}

func TestV3WatchReportsErrors(t *testing.T) {
	w := newFakeWatcher()
	b := &v3Backend{watcher: w, timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan clientv3.WatchResponse, 1)
	w.chans <- first
	out := b.watch(ctx, "/")
	first <- clientv3.WatchResponse{Canceled: true}
	if ev := recv(t, out); ev.Err == nil {
		t.Error("a cancelled watch must surface its error")
	}
	// Drain: the goroutine must be gone before the next test touches
	// watchRetry, and it must actually exit on cancel.
	cancel()
	for range out {
	}
}

func TestV3WatchWithoutClientFailsCleanly(t *testing.T) {
	b := &v3Backend{timeout: time.Second}
	out := b.watch(context.Background(), "/")
	if ev := recv(t, out); ev.Err == nil {
		t.Error("want an error event")
	}
	if _, ok := <-out; ok {
		t.Error("channel should close")
	}
}

type fakeV2Watcher struct{ results chan error }

func (w *fakeV2Watcher) Next(ctx context.Context) (*clientv2.Response, error) {
	select {
	case err := <-w.results:
		return &clientv2.Response{}, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestV2WatchEventsErrorsAndIndexCleared(t *testing.T) {
	old := watchRetry
	watchRetry = time.Millisecond
	defer func() { watchRetry = old }()

	fw := &fakeV2Watcher{results: make(chan error)}
	api := &casKeysAPI{fakeKeysAPI: &fakeKeysAPI{}, watcher: fw}
	b := &v2Backend{api: api, timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	out := b.watch(ctx, "/app/")

	fw.results <- nil
	if ev := recv(t, out); ev.Err != nil {
		t.Errorf("change → %v, want a refresh", ev.Err)
	}
	fw.results <- clientv2.Error{Code: clientv2.ErrorCodeEventIndexCleared}
	if ev := recv(t, out); ev.Err != nil {
		t.Errorf("index cleared → %v, want a refresh (resync), not an error", ev.Err)
	}
	fw.results <- errors.New("connection refused")
	if ev := recv(t, out); ev.Err == nil {
		t.Error("transport failure must surface as an error")
	}
	// …and the watch keeps going afterwards.
	fw.results <- nil
	if ev := recv(t, out); ev.Err != nil {
		t.Errorf("after recovery: %v", ev.Err)
	}
	cancel()
	for range out {
	}
}

// revKV reports whatever store revision the test sets, so the watch's
// "cluster went backwards" check can be driven.
type revKV struct {
	*fakeKV
	rev chan int64
	cur int64
}

func (k *revKV) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	select {
	case r := <-k.rev:
		k.cur = r
	default:
	}
	return &clientv3.GetResponse{Header: &etcdserverpb.ResponseHeader{Revision: k.cur}}, nil
}

// E2E round 2: a cluster recreated from scratch (or restored from an older
// backup) has a lower revision than the watch has seen; the client resumes at
// the old revision and the watch goes silent forever. It must notice, ask
// for a full refresh, and start over from the present.
func TestV3WatchRestartsWhenClusterRevisionGoesBackwards(t *testing.T) {
	oldR, oldC := watchRetry, watchCheck
	watchRetry, watchCheck = time.Millisecond, 20*time.Millisecond
	defer func() { watchRetry, watchCheck = oldR, oldC }()

	kv := &revKV{fakeKV: newFakeKV(), rev: make(chan int64, 1), cur: 500}
	w := newFakeWatcher()
	b := &v3Backend{cli: kv, watcher: w, timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan clientv3.WatchResponse, 1)
	w.chans <- first
	out := b.watch(ctx, "/")
	<-w.prefixes
	<-w.revs

	first <- clientv3.WatchResponse{Header: etcdserverpb.ResponseHeader{Revision: 510},
		Events: []*clientv3.Event{{Type: mvccpb.PUT, Kv: &mvccpb.KeyValue{Key: []byte("/x")}}}}
	recv(t, out)

	// The cluster is recreated: its revision is now 3.
	w.chans <- make(chan clientv3.WatchResponse)
	kv.rev <- 3
	if ev := recv(t, out); ev.Err != nil {
		t.Fatalf("want a refresh, got %v", ev.Err)
	}
	<-w.prefixes
	if r := <-w.revs; r != 0 {
		t.Errorf("restarted at rev %d, want the present (0), not the stale resume point", r)
	}
	cancel()
	for range out {
	}
}

// ---- v2 gaps (coverage review) ---------------------------------------------

func TestV2ExportFlattensSubtreeAndSkipsDirs(t *testing.T) {
	api := &fakeKeysAPI{gets: map[string]*clientv2.Response{
		"/app": {Node: &clientv2.Node{Key: "/app", Dir: true, Nodes: clientv2.Nodes{
			{Key: "/app/a", Value: "1"},
			{Key: "/app/sub", Dir: true, Nodes: clientv2.Nodes{
				{Key: "/app/sub/b", Value: "2"},
				{Key: "/app/sub/empty", Dir: true},
			}},
		}}},
	}}
	b := &v2Backend{api: api, timeout: time.Second}
	got, err := b.export("/app/", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["/app/a"] != "1" || got["/app/sub/b"] != "2" {
		t.Errorf("export = %v", got)
	}
	if got, err := b.export("/missing", 0); err != nil || len(got) != 0 {
		t.Errorf("missing dir export = %v, %v; want empty, nil", got, err)
	}
	if _, err := b.export("/app", 5); err == nil {
		t.Error("v2 export at a revision must be refused")
	}
}

func TestV2LsMapsTTLAndDirs(t *testing.T) {
	api := &fakeKeysAPI{gets: map[string]*clientv2.Response{
		"/": {ClusterID: "c1", Node: &clientv2.Node{Key: "/", Dir: true, Nodes: clientv2.Nodes{
			{Key: "/d", Dir: true},
			{Key: "/k", Value: "v", TTL: 42},
		}}},
	}}
	b := &v2Backend{api: api, timeout: time.Second}
	nodes, err := b.ls("/", 0)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("ls = %v, %v", nodes, err)
	}
	if !nodes[0].IsDir || nodes[1].TTL != 42 || nodes[1].ClusterId != "c1" {
		t.Errorf("ls nodes = %+v %+v", nodes[0], nodes[1])
	}
}

// The v2 watch must watch the directory key itself, recursively — the v2
// store's tree is real, so "/app" cannot match "/apple".
func TestV2WatchUsesNormalisedDirRecursively(t *testing.T) {
	var gotKey string
	var gotOpts *clientv2.WatcherOptions
	api := &recWatchAPI{fakeKeysAPI: &fakeKeysAPI{}, onWatch: func(k string, o *clientv2.WatcherOptions) {
		gotKey, gotOpts = k, o
	}}
	b := &v2Backend{api: api, timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	out := b.watch(ctx, "/app/")
	time.Sleep(50 * time.Millisecond)
	cancel()
	for range out {
	}
	if gotKey != "/app" || gotOpts == nil || !gotOpts.Recursive {
		t.Errorf("watched %q opts %+v, want /app recursive", gotKey, gotOpts)
	}
}

type recWatchAPI struct {
	*fakeKeysAPI
	onWatch func(string, *clientv2.WatcherOptions)
}

func (f *recWatchAPI) Watcher(k string, o *clientv2.WatcherOptions) clientv2.Watcher {
	f.onWatch(k, o)
	return &fakeV2Watcher{results: make(chan error)}
}
