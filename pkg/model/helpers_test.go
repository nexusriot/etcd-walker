package model

import (
	"errors"
	"testing"

	clientv2 "github.com/coreos/etcd/client"
)

func TestNormPath(t *testing.T) {
	cases := map[string]string{
		"":            "/",
		"/":           "/",
		"foo":         "/foo",
		"/foo/":       "/foo",
		"/foo//bar/":  "/foo/bar",
		"///a///b///": "/a/b",
	}
	for in, want := range cases {
		if got := normPath(in); got != want {
			t.Errorf("normPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithTrail(t *testing.T) {
	cases := map[string]string{
		"":         "/",
		"/":        "/",
		"foo":      "/foo/",
		"/foo":     "/foo/",
		"/foo/":    "/foo/",
		"/foo//ba": "/foo/ba/",
	}
	for in, want := range cases {
		if got := withTrail(in); got != want {
			t.Errorf("withTrail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnderOrEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"/a", "/a", true},
		{"/a", "/a/b", true},
		{"/a", "/ab", false}, // sibling with common prefix is NOT nested
		{"/", "/x", true},
		{"/a/b", "/a", false},
	}
	for _, c := range cases {
		if got := underOrEqual(c.a, c.b); got != c.want {
			t.Errorf("underOrEqual(%q, %q) = %t, want %t", c.a, c.b, got, c.want)
		}
	}
}

func TestDirGuards(t *testing.T) {
	if err := renameDirGuard("/a", "/a/b"); err == nil {
		t.Error("rename into own subtree must be rejected")
	}
	if err := renameDirGuard("/a/b", "/a"); err == nil {
		t.Error("rename onto own parent must be rejected")
	}
	if err := renameDirGuard("/a", "/b"); err != nil {
		t.Errorf("sibling rename should pass: %v", err)
	}
	if err := copyDirGuard("/a", "/a/b"); err == nil {
		t.Error("copy into own subtree must be rejected")
	}
	if err := copyDirGuard("/a", "/a-copy"); err != nil {
		t.Errorf("copy to prefix-sharing sibling should pass: %v", err)
	}
}

func TestIsAuthRequiredErr(t *testing.T) {
	if isAuthRequiredErr(nil) {
		t.Error("nil error must not be auth-required")
	}
	for _, msg := range []string{
		"etcdserver: user name is empty",
		"authentication required",
		"etcdserver: permission denied",
	} {
		if !isAuthRequiredErr(errors.New(msg)) {
			t.Errorf("expected auth-required for %q", msg)
		}
	}
	if isAuthRequiredErr(errors.New("connection refused")) {
		t.Error("connection refused must not be classified as auth-required")
	}
}

func TestV2CollectKeys(t *testing.T) {
	nodes := clientv2.Nodes{
		{Key: "/a", Value: "1"},
		{Key: "/d", Dir: true, Nodes: clientv2.Nodes{
			{Key: "/d/x", Value: "2"},
			{Key: "/d/sub", Dir: true, Nodes: clientv2.Nodes{
				{Key: "/d/sub/y", Value: "3"},
			}},
		}},
	}
	got := map[string]string{}
	v2collectKeys(nodes, got)
	want := map[string]string{"/a": "1", "/d/x": "2", "/d/sub/y": "3"}
	if len(got) != len(want) {
		t.Fatalf("v2collectKeys = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestNewModelAuthMisconfig(t *testing.T) {
	// password set, username empty => fail fast before any network call.
	_, err := NewModel(Options{Host: "127.0.0.1", Port: "2379", Password: "secret"})
	if err == nil {
		t.Fatal("expected error when password is set but username is empty")
	}
}
