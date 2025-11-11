package model

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	// v3 client
	clientv3 "go.etcd.io/etcd/client/v3"
	// v2 client
	clientv2 "github.com/coreos/etcd/client"
)

// ---------------- Public types ----------------

type Model struct {
	backend backend
}

type Node struct {
	Name      string
	ClusterId string
	IsDir     bool
	Value     string
}

// ProtocolVersion reports which backend is in use ("v2" or "v3").
func (m *Model) ProtocolVersion() string { return m.backend.proto() }

// Public API bridged to the selected backend.
func (m *Model) Ls(directory string) ([]*Node, error)  { return m.backend.ls(directory) }
func (m *Model) Get(key string) (*Node, error)         { return m.backend.get(key) } // used by Jump
func (m *Model) Set(key, value string) error           { return m.backend.set(key, value) }
func (m *Model) MkDir(directory string) error          { return m.backend.mkdir(directory) }
func (m *Model) Del(key string) error                  { return m.backend.del(key) }
func (m *Model) DelDir(key string) error               { return m.backend.deldir(key) }
func (m *Model) RenameDir(oldDir, newDir string) error { return m.backend.renameDir(oldDir, newDir) }

// ---------------- Backend interface ----------------

type backend interface {
	proto() string
	ls(directory string) ([]*Node, error)
	get(key string) (*Node, error)
	set(key, value string) error
	mkdir(directory string) error
	del(key string) error
	deldir(key string) error
	renameDir(oldDir, newDir string) error
}

// ---------------- NewModel with explicit protocol ----------------

func NewModel(host, port, protocol string) *Model {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "v3":
		b3, err := newV3Backend(host, port)
		if err != nil {
			panic(fmt.Sprintf("v3 init failed: %v", err))
		}
		if _, err := b3.ls("/"); err != nil {
			panic(fmt.Sprintf("v3 probe failed: %v", err))
		}
		return &Model{backend: b3}
	case "auto":
		if b3, err := newV3Backend(host, port); err == nil {
			if _, err := b3.ls("/"); err == nil {
				return &Model{backend: b3}
			}
		}
		if b2, err := newV2Backend(host, port); err == nil {
			if _, err := b2.ls("/"); err == nil {
				return &Model{backend: b2}
			}
		}
		panic("failed to connect using auto: neither v3 nor v2 worked")
	default: // "v2"
		b2, err := newV2Backend(host, port)
		if err != nil {
			panic(fmt.Sprintf("v2 init failed: %v", err))
		}
		if _, err := b2.ls("/"); err != nil {
			panic(fmt.Sprintf("v2 probe failed: %v", err))
		}
		return &Model{backend: b2}
	}
}

// ============================================================================
// v3 backend (prefix-based “dirs” with .dir marker) + normalization
// ============================================================================

type v3Backend struct {
	cli clientv3.KV
	c   *clientv3.Client
}

func newV3Backend(host, port string) (*v3Backend, error) {
	c, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{fmt.Sprintf("http://%s:%s", host, port)},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return &v3Backend{cli: clientv3.NewKV(c), c: c}, nil
}

func (b *v3Backend) proto() string { return "v3" }

const dirMarker = ".dir"

// ---- helpers for v3 ----
func normPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

func withTrail(p string) string {
	p = normPath(p)
	if p == "/" {
		return "/"
	}
	return strings.TrimSuffix(p, "/") + "/"
}

func (b *v3Backend) ls(directory string) ([]*Node, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prefix := withTrail(directory)
	resp, err := b.cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	clusterID := fmt.Sprintf("%d", resp.Header.GetClusterId())

	type childInfo struct {
		isDir     bool
		hasFile   bool
		fileValue string
	}
	children := map[string]*childInfo{}

	for _, kv := range resp.Kvs {
		key := normPath(string(kv.Key))
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		rest = strings.TrimLeft(rest, "/") // tolerate legacy double slashes
		if rest == "" {
			continue
		}
		parts := strings.SplitN(rest, "/", 2)
		child := parts[0]
		if child == "" || child == dirMarker {
			continue
		}
		ci := children[child]
		if ci == nil {
			ci = &childInfo{}
			children[child] = ci
		}
		if len(parts) == 2 {
			ci.isDir = true
			if parts[1] == dirMarker {
				ci.isDir = true
			}
		} else {
			ci.hasFile = true
			ci.fileValue = string(kv.Value)
		}
	}

	names := make([]string, 0, len(children))
	for k := range children {
		names = append(names, k)
	}
	sort.Strings(names)

	var nodes []*Node
	root := strings.TrimSuffix(prefix, "/")
	for _, name := range names {
		ci := children[name]
		full := root + "/" + name

		// If both exist, return BOTH entries: directory and file.
		if ci.isDir {
			nodes = append(nodes, &Node{Name: full, IsDir: true, ClusterId: clusterID})
		}
		if ci.hasFile {
			nodes = append(nodes, &Node{Name: full, IsDir: false, Value: ci.fileValue, ClusterId: clusterID})
		}
	}
	return nodes, nil
}

func (b *v3Backend) set(key, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key = normPath(key)
	_, err := b.cli.Put(ctx, key, value)
	return err
}

func (b *v3Backend) mkdir(directory string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := strings.TrimSuffix(normPath(directory), "/")
	marker := dir + "/" + dirMarker
	pfx := dir + "/"
	resp, err := b.cli.Get(ctx, pfx, clientv3.WithPrefix(), clientv3.WithLimit(1))
	if err != nil {
		return err
	}
	if resp.Count > 0 {
		return nil
	}
	_, err = b.cli.Put(ctx, marker, "")
	return err
}

func (b *v3Backend) del(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key = normPath(key)
	_, err := b.cli.Delete(ctx, key)
	return err
}

func (b *v3Backend) deldir(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pfx := withTrail(key)
	_, err := b.cli.Delete(ctx, pfx, clientv3.WithPrefix())
	return err
}

func (b *v3Backend) renameDir(oldDir, newDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	oldPfx := withTrail(oldDir)
	newPfx := withTrail(newDir)
	if oldPfx == newPfx {
		return nil
	}

	src, err := b.cli.Get(ctx, oldPfx, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	if src.Count == 0 {
		return fmt.Errorf("source does not exist: %s", oldDir)
	}
	dst, err := b.cli.Get(ctx, newPfx, clientv3.WithPrefix(), clientv3.WithLimit(1))
	if err != nil {
		return err
	}
	if dst.Count > 0 {
		return fmt.Errorf("target already exists: %s", newDir)
	}

	for _, kv := range src.Kvs {
		oldKey := normPath(string(kv.Key))
		newKey := strings.Replace(oldKey, oldPfx, newPfx, 1)
		if _, err := b.cli.Put(ctx, newKey, string(kv.Value)); err != nil {
			return fmt.Errorf("copy %s -> %s failed: %w", oldKey, newKey, err)
		}
	}
	_, err = b.cli.Delete(ctx, oldPfx, clientv3.WithPrefix())
	return err
}

// v3 Get: return file node if exact key exists,
// else return directory node if any key exists under the path (prefix match).
func (b *v3Backend) get(key string) (*Node, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	raw := key
	k := normPath(raw)

	exact, err := b.cli.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	if exact.Count > 0 {
		// There could be multiple KVs if compaction/revision, but the first is fine for value.
		kv := exact.Kvs[0]
		return &Node{
			Name:      k,
			ClusterId: fmt.Sprintf("%d", exact.Header.GetClusterId()),
			IsDir:     false,
			Value:     string(kv.Value),
		}, nil
	}

	// Try directory existence by prefix (any child or .dir marker)
	pfx := withTrail(k)
	dirProbe, err := b.cli.Get(ctx, pfx, clientv3.WithPrefix(), clientv3.WithLimit(1))
	if err != nil {
		return nil, err
	}
	if dirProbe.Count > 0 {
		return &Node{
			Name:      k, // directory logical name (no trailing slash)
			ClusterId: fmt.Sprintf("%d", dirProbe.Header.GetClusterId()),
			IsDir:     true,
			Value:     "",
		}, nil
	}

	return nil, fmt.Errorf("not found: %s", k)
}

// ============================================================================
// v2 backend (original KeysAPI logic)
// ============================================================================

type v2Backend struct {
	api    clientv2.KeysAPI
	client clientv2.Client
}

func newV2Backend(host, port string) (*v2Backend, error) {
	cli, err := clientv2.New(clientv2.Config{
		Endpoints: []string{fmt.Sprintf("http://%s:%s", host, port)},
	})
	if err != nil {
		return nil, err
	}
	return &v2Backend{api: clientv2.NewKeysAPI(cli), client: cli}, nil
}

func (b *v2Backend) proto() string { return "v2" }

func (b *v2Backend) ls(directory string) ([]*Node, error) {
	options := &clientv2.GetOptions{Sort: true, Recursive: false}
	resp, err := b.api.Get(context.Background(), directory, options)
	if err != nil {
		if clientv2.IsKeyNotFound(err) {
			return []*Node{}, nil
		}
		return []*Node{}, err
	}
	var nds []*Node
	for _, n := range resp.Node.Nodes {
		nds = append(nds, &Node{
			Name:      n.Key,
			ClusterId: resp.ClusterID,
			IsDir:     n.Dir,
			Value:     n.Value,
		})
	}
	return nds, nil
}

func (b *v2Backend) get(key string) (*Node, error) {
	ctx := context.Background()

	k := strings.TrimSpace(key)
	if k == "" {
		k = "/"
	}
	if k != "/" {
		k = strings.TrimRight(k, "/")
	}
	resp, err := b.api.Get(ctx, k, nil)
	if err != nil {
		return nil, err
	}
	return &Node{
		Name:      resp.Node.Key,
		ClusterId: resp.ClusterID,
		IsDir:     resp.Node.Dir,
		Value:     resp.Node.Value,
	}, nil
}

func (b *v2Backend) set(key, value string) error {
	_, err := b.api.Set(context.Background(), key, value, nil)
	return err
}

func (b *v2Backend) mkdir(directory string) error {
	res, err := b.api.Get(context.Background(), directory, nil)
	if err != nil && !clientv2.IsKeyNotFound(err) {
		return err
	}
	if err != nil && clientv2.IsKeyNotFound(err) {
		_, err = b.api.Set(context.Background(), directory, "", &clientv2.SetOptions{Dir: true, PrevExist: clientv2.PrevIgnore})
		return err
	}
	if !res.Node.Dir {
		return fmt.Errorf("trying to rewrite existing value with dir: %v", directory)
	}
	return nil
}

func (b *v2Backend) del(key string) error {
	_, err := b.api.Delete(context.Background(), key, nil)
	if err != nil && !clientv2.IsKeyNotFound(err) {
		return err
	}
	return nil
}

func (b *v2Backend) deldir(key string) error {
	_, err := b.api.Delete(context.Background(), key, &clientv2.DeleteOptions{Dir: true, Recursive: true})
	if err != nil && !clientv2.IsKeyNotFound(err) {
		return err
	}
	return nil
}

func (b *v2Backend) renameDir(oldDir, newDir string) error {
	ctx := context.Background()
	if oldDir == newDir {
		return nil
	}
	srcResp, err := b.api.Get(ctx, oldDir, &clientv2.GetOptions{Recursive: true})
	if err != nil {
		return err
	}
	if !srcResp.Node.Dir {
		return fmt.Errorf("source is not a directory: %s", oldDir)
	}
	if _, err := b.api.Get(ctx, newDir, nil); err == nil {
		return fmt.Errorf("target already exists: %s", newDir)
	} else if !clientv2.IsKeyNotFound(err) {
		return err
	}
	if _, err := b.api.Set(ctx, newDir, "", &clientv2.SetOptions{Dir: true, PrevExist: clientv2.PrevIgnore}); err != nil {
		return err
	}

	var copyTree func(n *clientv2.Node) error
	copyTree = func(n *clientv2.Node) error {
		if n.Key == oldDir {
			for _, ch := range n.Nodes {
				if err := copyTree(ch); err != nil {
					return err
				}
			}
			return nil
		}
		targetKey := strings.Replace(n.Key, oldDir, newDir, 1)
		if n.Dir {
			if _, err := b.api.Set(ctx, targetKey, "", &clientv2.SetOptions{Dir: true, PrevExist: clientv2.PrevIgnore}); err != nil && !clientv2.IsKeyNotFound(err) {
				return err
			}
			for _, ch := range n.Nodes {
				if err := copyTree(ch); err != nil {
					return err
				}
			}
			return nil
		}
		_, err := b.api.Set(ctx, targetKey, n.Value, nil)
		return err
	}

	if err := copyTree(srcResp.Node); err != nil {
		return err
	}

	_, err = b.api.Delete(ctx, oldDir, &clientv2.DeleteOptions{Dir: true, Recursive: true})
	return err
}
