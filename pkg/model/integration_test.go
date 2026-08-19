//go:build integration

// Package-level integration tests against a real etcd. They are behind the
// `integration` build tag so the default `go test ./...` stays hermetic.
//
//	go test ./pkg/model -tags=integration -run Integration -v
//
// Endpoint and credentials come from the environment:
//
//	ETCD_WALKER_TEST_ENDPOINT  host:port   (default 127.0.0.1:2379)
//	ETCD_WALKER_TEST_USER      username    (optional)
//	ETCD_WALKER_TEST_PASSWORD  password    (optional)
//
// Everything is written under a per-test prefix and torn down afterwards.
package model

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func integrationModel(t *testing.T) *Model {
	t.Helper()
	endpoint := os.Getenv("ETCD_WALKER_TEST_ENDPOINT")
	if endpoint == "" {
		endpoint = "127.0.0.1:2379"
	}
	host, port, ok := strings.Cut(endpoint, ":")
	if !ok {
		t.Fatalf("ETCD_WALKER_TEST_ENDPOINT %q is not host:port", endpoint)
	}
	m, err := NewModel(Options{
		Host:           host,
		Port:           port,
		Protocol:       "v3",
		Username:       os.Getenv("ETCD_WALKER_TEST_USER"),
		Password:       os.Getenv("ETCD_WALKER_TEST_PASSWORD"),
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("connecting to %s: %v", endpoint, err)
	}
	return m
}

// scratch returns a unique prefix for one test and registers its cleanup.
func scratch(t *testing.T, m *Model) string {
	t.Helper()
	prefix := fmt.Sprintf("/etcd-walker-it/%s-%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() { _ = m.DelDir(prefix) })
	return prefix
}

// The CRUD round-trip the whole UI rests on, against a real server rather
// than the in-memory fake.
func TestIntegrationKeyLifecycle(t *testing.T) {
	m := integrationModel(t)
	p := scratch(t, m)

	if err := m.Set(p+"/k", "hello"); err != nil {
		t.Fatal(err)
	}
	n, err := m.Get(p + "/k")
	if err != nil {
		t.Fatal(err)
	}
	if n.Value != "hello" || n.IsDir {
		t.Fatalf("get = %+v, want the value we wrote", n)
	}
	if n.CreateRev == 0 || n.ModRev == 0 || n.Version == 0 {
		t.Errorf("revision metadata not populated: %+v", n)
	}

	if err := m.Set(p+"/k", "second"); err != nil {
		t.Fatal(err)
	}
	n2, _ := m.Get(p + "/k")
	if n2.ModRev <= n.ModRev {
		t.Errorf("ModRev did not advance: %d -> %d", n.ModRev, n2.ModRev)
	}

	if err := m.Del(p + "/k"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(p + "/k"); err == nil {
		t.Error("key still readable after Del")
	}
}

// Directories are synthetic on v3: a listing must split children into dirs and
// keys, hide the .dir marker, and see an empty directory created by MkDir.
func TestIntegrationListingAndDirectories(t *testing.T) {
	m := integrationModel(t)
	p := scratch(t, m)

	for k, v := range map[string]string{
		p + "/a":       "1",
		p + "/b":       "2",
		p + "/sub/c":   "3",
		p + "/sub/d/e": "4",
	} {
		if err := m.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.MkDir(p + "/empty"); err != nil {
		t.Fatal(err)
	}

	nodes, err := m.Ls(p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range nodes {
		got[n.Name] = n.IsDir
	}
	for name, wantDir := range map[string]bool{
		p + "/a": false, p + "/b": false, p + "/sub": true, p + "/empty": true,
	} {
		isDir, present := got[name]
		if !present {
			t.Errorf("%s missing from listing: %+v", name, got)
			continue
		}
		if isDir != wantDir {
			t.Errorf("%s IsDir = %v, want %v", name, isDir, wantDir)
		}
	}
	if _, leaked := got[p+"/.dir"]; leaked {
		t.Error("the .dir marker leaked into the listing")
	}
	// Listings are keys-only: values arrive via Get, not Ls.
	for _, n := range nodes {
		if !n.IsDir && n.Value != "" {
			t.Errorf("listing carried a value for %s — Ls should be keys-only", n.Name)
		}
	}
}

// The prefix boundary on a recursive delete, against real etcd range
// semantics: a sibling sharing a string prefix must survive.
func TestIntegrationDeldirPrefixBoundary(t *testing.T) {
	m := integrationModel(t)
	p := scratch(t, m)

	if err := m.Set(p+"/app/k", "1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(p+"/application", "2"); err != nil {
		t.Fatal(err)
	}

	if err := m.DelDir(p + "/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(p + "/app/k"); err == nil {
		t.Error("/app/k survived the recursive delete")
	}
	if n, err := m.Get(p + "/application"); err != nil || n.Value != "2" {
		t.Errorf("deldir crossed the prefix boundary and took /application: %v / %+v", err, n)
	}
}

// B-1, against a real lease: a copy shares its source's lease, so changing one
// key's TTL must not revoke the lease out from under the other.
func TestIntegrationSharedLeaseSurvivesTTLChange(t *testing.T) {
	m := integrationModel(t)
	p := scratch(t, m)

	if err := m.SetTTL(p+"/orig", "v", 300); err != nil {
		t.Fatal(err)
	}
	orig, err := m.Get(p + "/orig")
	if err != nil {
		t.Fatal(err)
	}
	if orig.LeaseID == 0 || orig.TTL <= 0 {
		t.Fatalf("SetTTL did not attach a live lease: %+v", orig)
	}

	// CopyKey re-attaches the source's lease — that is what makes it shared.
	if err := m.CopyKey(p+"/orig", p+"/copy"); err != nil {
		t.Fatal(err)
	}
	cp, err := m.Get(p + "/copy")
	if err != nil {
		t.Fatal(err)
	}
	if cp.LeaseID != orig.LeaseID {
		t.Fatalf("copy lease %d != source lease %d — precondition for this test", cp.LeaseID, orig.LeaseID)
	}

	// Re-TTL the original. The old lease is now shared, so it must NOT be revoked.
	if err := m.SetTTL(p+"/orig", "v", 600); err != nil {
		t.Fatal(err)
	}
	survivor, err := m.Get(p + "/copy")
	if err != nil {
		t.Fatalf("the shared-lease copy was DELETED by a TTL change on its sibling: %v", err)
	}
	if survivor.Value != "v" {
		t.Errorf("copy value = %q, want v", survivor.Value)
	}
}

// A lease nothing else uses must still be reaped, or every TTL change leaks one.
func TestIntegrationExclusiveLeaseIsRevoked(t *testing.T) {
	m := integrationModel(t)
	p := scratch(t, m)

	if err := m.SetTTL(p+"/solo", "v", 300); err != nil {
		t.Fatal(err)
	}
	first, _ := m.Get(p + "/solo")
	if err := m.SetTTL(p+"/solo", "v", 600); err != nil {
		t.Fatal(err)
	}
	second, err := m.Get(p + "/solo")
	if err != nil {
		t.Fatal(err)
	}
	if second.LeaseID == first.LeaseID {
		t.Errorf("expected a fresh lease, still on %d", first.LeaseID)
	}
	if second.TTL <= 0 {
		t.Errorf("re-TTL left the key without a live expiry: %+v", second)
	}
	// Editing the value must keep that expiry running.
	if err := m.SetKeepTTL(p+"/solo", "edited", second.LeaseID, second.TTL); err != nil {
		t.Fatal(err)
	}
	after, _ := m.Get(p + "/solo")
	if after.Value != "edited" || after.LeaseID != second.LeaseID {
		t.Errorf("SetKeepTTL broke the lease: %+v", after)
	}
}

// The MVCC walk behind Ctrl+V, against real revisions.
func TestIntegrationRevisionHistory(t *testing.T) {
	m := integrationModel(t)
	p := scratch(t, m)

	for _, v := range []string{"one", "two", "three"} {
		if err := m.Set(p+"/h", v); err != nil {
			t.Fatal(err)
		}
	}
	revs, truncated, err := m.History(p+"/h", 10)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("3 revisions should not hit a limit of 10")
	}
	if len(revs) != 3 {
		t.Fatalf("got %d revisions, want 3: %+v", len(revs), revs)
	}
	// Newest first, and the walk must reach the creating write.
	if revs[0].Value != "three" || revs[len(revs)-1].Value != "one" {
		t.Errorf("history order wrong: %q … %q", revs[0].Value, revs[len(revs)-1].Value)
	}
	if revs[len(revs)-1].Version != 1 {
		t.Errorf("oldest revision Version = %d, want 1 (the creating write)", revs[len(revs)-1].Version)
	}

	if _, _, err := m.History(p+"/never-existed", 10); err == nil {
		t.Error("history of a missing key should error")
	}
}

// Export/Import round-trip, including the reserved-marker guard.
func TestIntegrationExportImportRoundTrip(t *testing.T) {
	m := integrationModel(t)
	src := scratch(t, m)
	dst := scratch(t, m)

	for k, v := range map[string]string{
		src + "/a":     "1",
		src + "/sub/b": "2",
	} {
		if err := m.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.MkDir(src + "/emptydir"); err != nil {
		t.Fatal(err)
	}

	dump, err := m.Export(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(dump) != 2 {
		t.Fatalf("export = %v, want exactly the two real keys (markers omitted)", dump)
	}

	// Re-key the dump under the destination and import it back.
	moved := map[string]string{}
	for k, v := range dump {
		moved[dst+strings.TrimPrefix(k, src)] = v
	}
	// A marker smuggled into the payload must be skipped, not written.
	moved[dst+"/.dir"] = ""

	written, skipped, err := m.Import(moved, true)
	if err != nil {
		t.Fatal(err)
	}
	if written != 2 || skipped != 1 {
		t.Errorf("import wrote %d / skipped %d, want 2 / 1 (the marker)", written, skipped)
	}
	if n, err := m.Get(dst + "/sub/b"); err != nil || n.Value != "2" {
		t.Errorf("round-tripped key missing: %v / %+v", err, n)
	}
	if _, err := m.Get(dst + "/.dir"); err == nil {
		t.Error("import wrote the reserved .dir marker")
	}
}

// Recursive find over a real keyspace, including the value-matching mode.
func TestIntegrationSearch(t *testing.T) {
	m := integrationModel(t)
	p := scratch(t, m)

	if err := m.Set(p+"/alpha", "needle here"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(p+"/beta", "nothing"); err != nil {
		t.Fatal(err)
	}

	byPath, _, err := m.Search(p, "alph", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(byPath) != 1 || byPath[0].Name != p+"/alpha" {
		t.Errorf("path search = %+v, want just /alpha", byPath)
	}

	byValue, _, err := m.Search(p, "needle", true, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(byValue) != 1 || byValue[0].Name != p+"/alpha" {
		t.Errorf("value search = %+v, want just /alpha", byValue)
	}
	if none, _, _ := m.Search(p, "needle", false, 50); len(none) != 0 {
		t.Errorf("path-only search matched a value: %+v", none)
	}
}
