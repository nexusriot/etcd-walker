package model

import (
	"fmt"
	"reflect"
	"testing"
)

// recordingBackend is a backend that records the method it was asked for and
// the arguments it received, so the thin Model wrappers can be checked for
// wiring rather than behaviour.
type recordingBackend struct {
	call string
	args []interface{}
}

func (r *recordingBackend) note(name string, args ...interface{}) {
	r.call, r.args = name, args
}

func (r *recordingBackend) proto() string { r.note("proto"); return "vX" }
func (r *recordingBackend) probe() error  { r.note("probe"); return nil }
func (r *recordingBackend) ls(dir string, rev int64) ([]*Node, error) {
	r.note("ls", dir, rev)
	return nil, nil
}
func (r *recordingBackend) get(key string, rev int64) (*Node, error) {
	r.note("get", key, rev)
	return nil, nil
}
func (r *recordingBackend) revision() (int64, error)    { r.note("revision"); return 7, nil }
func (r *recordingBackend) set(key, value string) error { r.note("set", key, value); return nil }
func (r *recordingBackend) setTTL(key, value string, ttl int64) error {
	r.note("setTTL", key, value, ttl)
	return nil
}
func (r *recordingBackend) setKeep(key, value string, leaseID, ttl int64) error {
	r.note("setKeep", key, value, leaseID, ttl)
	return nil
}
func (r *recordingBackend) mkdir(dir string) error  { r.note("mkdir", dir); return nil }
func (r *recordingBackend) del(key string) error    { r.note("del", key); return nil }
func (r *recordingBackend) deldir(key string) error { r.note("deldir", key); return nil }
func (r *recordingBackend) renameDir(o, n string) error {
	r.note("renameDir", o, n)
	return nil
}
func (r *recordingBackend) renameKey(o, n string) error {
	r.note("renameKey", o, n)
	return nil
}
func (r *recordingBackend) copyKey(s, d string) error { r.note("copyKey", s, d); return nil }
func (r *recordingBackend) copyDir(s, d string) error { r.note("copyDir", s, d); return nil }
func (r *recordingBackend) search(dir, q string, inValues bool, limit int, rev int64) ([]*Node, bool, error) {
	r.note("search", dir, q, inValues, limit, rev)
	return nil, false, nil
}
func (r *recordingBackend) history(key string, limit int) ([]*Revision, bool, error) {
	r.note("history", key, limit)
	return nil, false, nil
}
func (r *recordingBackend) authStatus() (bool, bool, error) {
	r.note("authStatus")
	return false, false, nil
}
func (r *recordingBackend) export(dir string, rev int64) (map[string]string, error) {
	r.note("export", dir, rev)
	return nil, nil
}

// Model's public methods are one-line delegations. Nothing else tests them,
// because the suites drive the backends directly — which means a transposed
// pair (Del↔DelDir, CopyKey↔CopyDir, RenameKey↔RenameDir) would compile, pass
// every existing test, and silently destroy data. Pin the wiring.
func TestModelDelegatesToBackend(t *testing.T) {
	cases := []struct {
		name     string
		invoke   func(m *Model)
		wantCall string
		wantArgs []interface{}
	}{
		{"Ls", func(m *Model) { m.Ls("/d") }, "ls", []interface{}{"/d", int64(0)}},
		{"Get", func(m *Model) { m.Get("/k") }, "get", []interface{}{"/k", int64(0)}},
		{"Set", func(m *Model) { m.Set("/k", "v") }, "set", []interface{}{"/k", "v"}},
		{"SetTTL", func(m *Model) { m.SetTTL("/k", "v", 60) }, "setTTL", []interface{}{"/k", "v", int64(60)}},
		{"SetKeepTTL", func(m *Model) { m.SetKeepTTL("/k", "v", 7, 30) }, "setKeep", []interface{}{"/k", "v", int64(7), int64(30)}},
		{"MkDir", func(m *Model) { m.MkDir("/d") }, "mkdir", []interface{}{"/d"}},
		{"Del", func(m *Model) { m.Del("/k") }, "del", []interface{}{"/k"}},
		{"DelDir", func(m *Model) { m.DelDir("/d") }, "deldir", []interface{}{"/d"}},
		{"RenameDir", func(m *Model) { m.RenameDir("/a", "/b") }, "renameDir", []interface{}{"/a", "/b"}},
		{"RenameKey", func(m *Model) { m.RenameKey("/a", "/b") }, "renameKey", []interface{}{"/a", "/b"}},
		{"CopyKey", func(m *Model) { m.CopyKey("/a", "/b") }, "copyKey", []interface{}{"/a", "/b"}},
		{"CopyDir", func(m *Model) { m.CopyDir("/a", "/b") }, "copyDir", []interface{}{"/a", "/b"}},
		{"Export", func(m *Model) { m.Export("/d") }, "export", []interface{}{"/d", int64(0)}},
		{"Search", func(m *Model) { m.Search("/d", "q", true, 5) }, "search", []interface{}{"/d", "q", true, 5, int64(0)}},
		{"History", func(m *Model) { m.History("/k", 9) }, "history", []interface{}{"/k", 9}},
		{"ProtocolVersion", func(m *Model) { m.ProtocolVersion() }, "proto", nil},
		// The *At readers must pass the revision through untouched: a dropped
		// rev would silently serve current data under a historical label.
		{"LsAt", func(m *Model) { m.LsAt("/d", 42) }, "ls", []interface{}{"/d", int64(42)}},
		{"GetAt", func(m *Model) { m.GetAt("/k", 42) }, "get", []interface{}{"/k", int64(42)}},
		{"ExportAt", func(m *Model) { m.ExportAt("/d", 42) }, "export", []interface{}{"/d", int64(42)}},
		{"SearchAt", func(m *Model) { m.SearchAt("/d", "q", true, 5, 42) }, "search", []interface{}{"/d", "q", true, 5, int64(42)}},
		{"Revision", func(m *Model) { m.Revision() }, "revision", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingBackend{}
			tc.invoke(&Model{backend: r})
			if r.call != tc.wantCall {
				t.Fatalf("Model.%s called backend.%s, want backend.%s", tc.name, r.call, tc.wantCall)
			}
			if tc.wantArgs != nil && !reflect.DeepEqual(r.args, tc.wantArgs) {
				t.Errorf("Model.%s passed %#v, want %#v", tc.name, r.args, tc.wantArgs)
			}
		})
	}
}

// AuthLabel is the one wrapper with logic of its own, and it must tolerate a
// nil receiver: the controller reads it before it knows the model connected.
func TestModelAuthLabel(t *testing.T) {
	var nilModel *Model
	if got := nilModel.AuthLabel(); got != "?" {
		t.Errorf("nil Model AuthLabel = %q, want ?", got)
	}
	if got := (&Model{}).AuthLabel(); got != "?" {
		t.Errorf("empty label = %q, want ?", got)
	}
	if got := (&Model{authLabel: "ON"}).AuthLabel(); got != "ON" {
		t.Errorf("AuthLabel = %q, want ON", got)
	}
}

// Errors from the backend must reach the caller unchanged — a wrapper that
// swallowed one would make a failed write look successful.
func TestModelPropagatesBackendErrors(t *testing.T) {
	want := fmt.Errorf("etcdserver: permission denied")
	b := &failingBackend{err: want}
	m := &Model{backend: b}

	if err := m.Set("/k", "v"); err != want {
		t.Errorf("Set error = %v, want %v", err, want)
	}
	if err := m.Del("/k"); err != want {
		t.Errorf("Del error = %v, want %v", err, want)
	}
	if _, err := m.Get("/k"); err != want {
		t.Errorf("Get error = %v, want %v", err, want)
	}
}

type failingBackend struct {
	recordingBackend
	err error
}

func (f *failingBackend) set(string, string) error         { return f.err }
func (f *failingBackend) del(string) error                 { return f.err }
func (f *failingBackend) get(string, int64) (*Node, error) { return nil, f.err }
