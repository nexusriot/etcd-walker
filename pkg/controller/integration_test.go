//go:build integration

// Controller-level integration tests: the flows whose value depends on a real
// server round-trip rather than on what a fake was told to return.
//
//	go test ./pkg/controller -tags=integration -run Integration -v
//
// Endpoint and credentials come from the same environment variables the model
// suite uses. Everything is written under a per-test prefix and removed after.
package controller

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/etcd-walker/pkg/model"
)

func integrationController(t *testing.T) (*Controller, *model.Model) {
	t.Helper()
	endpoint := os.Getenv("ETCD_WALKER_TEST_ENDPOINT")
	if endpoint == "" {
		endpoint = "127.0.0.1:2379"
	}
	host, port, ok := strings.Cut(endpoint, ":")
	if !ok {
		t.Fatalf("ETCD_WALKER_TEST_ENDPOINT %q is not host:port", endpoint)
	}
	m, err := model.NewModel(model.Options{
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
	c := newTestController(m)
	c.policy.SnapshotBeforeDelete = true
	c.endpoint = endpoint // NewController does this; the test view does not
	return c, m
}

func integrationScratch(t *testing.T, m *model.Model) string {
	t.Helper()
	prefix := fmt.Sprintf("/etcd-walker-it/%s-%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() { _ = m.DelDir(prefix) })
	return prefix
}

// The undo story end to end: a recursive delete really removes the subtree
// from the cluster, the snapshot on disk really holds it, and restoring really
// puts it back. Each half is testable with a fake; that the same bytes survive
// the round trip through a real DeleteRange is not.
func TestIntegrationDeleteSnapshotRestoreRoundTrip(t *testing.T) {
	t.Setenv("ETCD_WALKER_STATE_DIR", t.TempDir())
	c, m := integrationController(t)
	p := integrationScratch(t, m)

	want := map[string]string{
		p + "/a":       "one",
		p + "/b":       "two",
		p + "/sub/c":   "three",
		p + "/binary":  "\x00\xff not text",
		p + "/spaces ": "value with spaces",
	}
	for k, v := range want {
		if err := m.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}

	c.currentDir = parentOf(p) + "/"
	c.updateList()
	c.deleteNode(&Node{node: &model.Node{Name: p, IsDir: true}})

	// The cluster no longer has the subtree.
	left, err := m.Export(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("keys survived the delete: %v", left)
	}

	// The snapshot does, byte for byte.
	snaps, err := listSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected one snapshot, got %d", len(snaps))
	}
	snap, err := readSnapshot(snaps[0].file)
	if err != nil {
		t.Fatal(err)
	}
	held, err := snap.values()
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if held[k] != v {
			t.Errorf("snapshot[%q] = %q, want %q", k, held[k], v)
		}
	}
	if snap.Revision == 0 {
		t.Error("snapshot did not record the cluster revision")
	}
	if snap.Endpoint == "" {
		t.Error("snapshot did not record which cluster it came from")
	}

	// And restoring puts every key back where it was.
	c.applyRestore(snap, true)
	back, err := m.Export(p)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if back[k] != v {
			t.Errorf("after restore %q = %q, want %q", k, back[k], v)
		}
	}
}

// A pane pinned to a past revision browses the real cluster's history: the
// listing, the details read and the export all have to agree with what the
// server had at that revision, including keys deleted since.
func TestIntegrationPinnedPaneBrowsesRealHistory(t *testing.T) {
	c, m := integrationController(t)
	p := integrationScratch(t, m)

	if err := m.Set(p+"/kept", "before"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(p+"/removed", "gone soon"); err != nil {
		t.Fatal(err)
	}
	rev, err := m.Revision()
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Set(p+"/kept", "after"); err != nil {
		t.Fatal(err)
	}
	if err := m.Del(p + "/removed"); err != nil {
		t.Fatal(err)
	}

	c.currentDir = p + "/"
	c.applyRevision(rev)
	if c.rev != rev {
		t.Fatalf("pane rev = %d, want %d", c.rev, rev)
	}
	if _, ok := c.currentNodes["removed|file"]; !ok {
		t.Errorf("the pinned listing lost a key that existed at rev %d: %v", rev, c.currentNodes)
	}

	// Filling the details re-reads the focused key; at a pinned revision that
	// read must return the old value, not today's.
	c.fillDetails("kept|file")
	if got := c.currentNodes["kept|file"].node.Value; got != "before" {
		t.Errorf("details read %q at rev %d, want the value from then", got, rev)
	}

	// Coming back to live shows the current tree again.
	c.applyRevision(0)
	if _, ok := c.currentNodes["removed|file"]; ok {
		t.Error("returning to live still lists the deleted key")
	}
	c.fillDetails("kept|file")
	if got := c.currentNodes["kept|file"].node.Value; got != "after" {
		t.Errorf("live details read %q, want after", got)
	}
}
