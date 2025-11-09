// pkg/model/model.go
package model

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type Model struct {
	client *clientv3.Client
	kv     clientv3.KV
}

type Node struct {
	Name      string
	ClusterId string
	IsDir     bool
	Value     string
}

// A hidden marker key to allow empty "directories" to exist in etcd v3.
const dirMarker = ".dir"

func NewModel(host string, port string) *Model {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{fmt.Sprintf("http://%s:%s", host, port)},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		panic(err)
	}
	return &Model{
		client: cli,
		kv:     clientv3.NewKV(cli),
	}
}

func (m *Model) appendNode(nodes []*Node, name string, isDirectory bool, value string, clusterId string) ([]*Node, error) {
	node := &Node{
		Name:      name,
		IsDir:     isDirectory,
		ClusterId: clusterId,
		Value:     value,
	}
	nodes = append(nodes, node)
	return nodes, nil
}

// Ls lists only the immediate children of "directory".
// "directory" should end with "/" (controller already does this).
// We treat keys with prefix "directory" as members; a child is:
//   - a "file" if there's a key exactly "directory + child"
//   - a "dir" if there's any key "directory + child/..." OR a marker "directory + child/.dir"
func (m *Model) Ls(directory string) ([]*Node, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Normalize: ensure trailing slash for prefix ranges.
	prefix := withTrailingSlash(directory)

	// Fetch keys+values once (we need values for direct child files).
	resp, err := m.kv.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}

	clusterID := fmt.Sprintf("%d", resp.Header.GetClusterId())

	// Maps to collect what immediate children exist
	type childInfo struct {
		isDir     bool
		hasFile   bool
		fileValue string // if direct child file exists
	}
	children := map[string]*childInfo{}

	for _, kv := range resp.Kvs {
		key := string(kv.Key)

		// Strip the directory prefix to analyze the child path
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		if rest == "" {
			// Key equals the directory itself; ignore (we never write such key)
			continue
		}
		// immediate child name is up to the first slash (if any)
		parts := strings.SplitN(rest, "/", 2)
		child := parts[0]
		if child == "" {
			continue
		}
		// skip our marker when listing the child (marker lives inside the child dir)
		if child == dirMarker {
			// This would be the unlikely case of writing "/path/.dir" directly.
			// We ignore it at parent level.
			continue
		}

		info := children[child]
		if info == nil {
			info = &childInfo{}
			children[child] = info
		}

		// If there is a remaining part after child/, then it's a directory
		if len(parts) == 2 {
			info.isDir = true
			// If that remaining tail is exactly ".dir" it also proves directory existence
			if parts[1] == dirMarker {
				info.isDir = true
			}
			continue
		}

		// No further slash → candidate file (direct child)
		// But also watch out for a child that equals ".dir" (dir marker at this level) – do not list as a file.
		if child == dirMarker {
			continue
		}
		info.hasFile = true
		info.fileValue = string(kv.Value)
	}

	// Build Node slice; stable order (controller sorts again, but keep deterministic)
	names := make([]string, 0, len(children))
	for k := range children {
		names = append(names, k)
	}
	sort.Strings(names)

	var nodes []*Node
	for _, name := range names {
		info := children[name]

		isDir := info.isDir
		// If both a file and deeper keys exist under the same child name,
		// treat it as a directory in UI (so “child/” appears).
		// Otherwise, if only a file exists, show as file.
		if isDir {
			// directory node, value ignored
			nodes, _ = m.appendNode(nodes, strings.TrimSuffix(prefix, "/")+"/"+name, true, "", clusterID)
		} else if info.hasFile {
			// file node
			nodes, _ = m.appendNode(nodes, strings.TrimSuffix(prefix, "/")+"/"+name, false, info.fileValue, clusterID)
		}
	}

	return nodes, nil
}

// Set writes a value key exactly as provided.
func (m *Model) Set(key, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.kv.Put(ctx, key, value)
	return err
}

// MkDir creates an "empty dir" by writing a hidden marker "<dir>/.dir" if nothing else exists.
// Accepts "directory" WITHOUT a trailing slash (controller passes this form).
func (m *Model) MkDir(directory string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Normalize (no trailing slash in arg), marker is dir + "/.dir"
	dir := strings.TrimSuffix(directory, "/")
	marker := dir + "/" + dirMarker

	// If something already exists under prefix dir+"/", we're done (it already exists)
	pfx := dir + "/"
	resp, err := m.kv.Get(ctx, pfx, clientv3.WithPrefix(), clientv3.WithLimit(1))
	if err != nil {
		return err
	}
	if resp.Count > 0 {
		// Directory already “exists”
		return nil
	}
	// Create marker so the empty directory shows up
	_, err = m.kv.Put(ctx, marker, "")
	return err
}

// Del deletes a single key (file).
func (m *Model) Del(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.kv.Delete(ctx, key)
	return err
}

// DelDir deletes everything under "key/" including the marker if present.
// Accepts "key" WITHOUT trailing slash (controller passes that).
func (m *Model) DelDir(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pfx := strings.TrimSuffix(key, "/") + "/"
	_, err := m.kv.Delete(ctx, pfx, clientv3.WithPrefix())
	return err
}

// RenameDir copies all keys from oldDir → newDir (both without trailing slash) then deletes oldDir recursively.
// Fails if newDir already exists (has any keys under newDir/).
func (m *Model) RenameDir(oldDir, newDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	oldPfx := strings.TrimSuffix(oldDir, "/") + "/"
	newPfx := strings.TrimSuffix(newDir, "/") + "/"

	if oldPfx == newPfx {
		return nil
	}

	// Ensure source exists
	srcResp, err := m.kv.Get(ctx, oldPfx, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	if srcResp.Count == 0 {
		return fmt.Errorf("source does not exist: %s", oldDir)
	}

	// Ensure target does not exist
	dstResp, err := m.kv.Get(ctx, newPfx, clientv3.WithPrefix(), clientv3.WithLimit(1))
	if err != nil {
		return err
	}
	if dstResp.Count > 0 {
		return fmt.Errorf("target already exists: %s", newDir)
	}

	// Copy all keys (including marker and deeper)
	for _, kv := range srcResp.Kvs {
		oldKey := string(kv.Key)
		newKey := strings.Replace(oldKey, oldPfx, newPfx, 1)
		if _, err := m.kv.Put(ctx, newKey, string(kv.Value)); err != nil {
			return fmt.Errorf("copy %s -> %s failed: %w", oldKey, newKey, err)
		}
	}

	// Delete old subtree
	if _, err := m.kv.Delete(ctx, oldPfx, clientv3.WithPrefix()); err != nil {
		return err
	}
	return nil
}

// withTrailingSlash ensures a trailing "/" unless the string is exactly "/".
func withTrailingSlash(p string) string {
	if p == "/" {
		return p
	}
	return strings.TrimSuffix(p, "/") + "/"
}
