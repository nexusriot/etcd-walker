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
