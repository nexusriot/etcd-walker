package model

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	// v3 client
	clientv3 "go.etcd.io/etcd/client/v3"
	// v2 client
	clientv2 "github.com/coreos/etcd/client"
)

type Model struct {
	backend backend
}

type Node struct {
	Name      string
	ClusterId string
	IsDir     bool
	Value     string
}

type Options struct {
	Host     string
	Port     string
	Protocol string // v2, v3, auto
	Username string
	Password string
}

func (m *Model) ProtocolVersion() string { return m.backend.proto() }

func (m *Model) Ls(directory string) ([]*Node, error)  { return m.backend.ls(directory) }
func (m *Model) Get(key string) (*Node, error)         { return m.backend.get(key) }
func (m *Model) Set(key, value string) error           { return m.backend.set(key, value) }
func (m *Model) MkDir(directory string) error          { return m.backend.mkdir(directory) }
func (m *Model) Del(key string) error                  { return m.backend.del(key) }
func (m *Model) DelDir(key string) error               { return m.backend.deldir(key) }
func (m *Model) RenameDir(oldDir, newDir string) error { return m.backend.renameDir(oldDir, newDir) }

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

func NewModel(opts Options) (*Model, error) {
	host, port := opts.Host, opts.Port

	if strings.TrimSpace(opts.Username) == "" && strings.TrimSpace(opts.Password) != "" {
		return nil, fmt.Errorf("auth misconfigured: password is set but username is empty (set --username or username in config)")
	}
	switch strings.ToLower(strings.TrimSpace(opts.Protocol)) {

	case "v3":
		b3, err := newV3Backend(host, port, opts.Username, opts.Password)
		if err != nil {
			return nil, fmt.Errorf("v3 init failed: %w", err)
		}
		if _, err := b3.ls("/"); err != nil {
			return nil, fmt.Errorf("v3 probe failed: %w", err)
		}
		return &Model{backend: b3}, nil

	case "auto":
		if b3, err := newV3Backend(host, port, opts.Username, opts.Password); err == nil {
			if _, err := b3.ls("/"); err == nil {
				return &Model{backend: b3}, nil
			} else if isAuthRequiredErr(err) {
				return nil, fmt.Errorf("etcd auth is enabled; provide --username/--password (or set them in config). Original: %w", err)
			}
		}
		if b2, err := newV2Backend(host, port, opts.Username, opts.Password); err == nil {
			if _, err := b2.ls("/"); err == nil {
				return &Model{backend: b2}, nil
			} else if isAuthRequiredErr(err) {
				return nil, fmt.Errorf("etcd auth is enabled; provide --username/--password (or set them in config). Original: %w", err)
			}
		}

		return nil, fmt.Errorf("auto: neither v3 nor v2 reachable at %s:%s", host, port)

	default: // v2
		b2, err := newV2Backend(host, port, opts.Username, opts.Password)
		if err != nil {
			return nil, fmt.Errorf("v2 init failed: %w", err)
		}
		if _, err := b2.ls("/"); err != nil {
			return nil, fmt.Errorf("v2 probe failed: %w", err)
		}
		return &Model{backend: b2}, nil
	}
}

type v3Backend struct {
	cli clientv3.KV
	c   *clientv3.Client
}

func isAuthRequiredErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "user name is empty") ||
		strings.Contains(s, "authentication required") ||
		strings.Contains(s, "permission denied")
}

func newV3Backend(host, port, username, password string) (*v3Backend, error) {
	cfg := clientv3.Config{
		Endpoints:   []string{fmt.Sprintf("http://%s:%s", host, port)},
		DialTimeout: 5 * time.Second,
		Logger:      zap.NewNop(),
	}
	if username != "" {
		cfg.Username = username
		cfg.Password = password
	}

	c, err := clientv3.New(cfg)
	if err != nil {
		return nil, err
	}
	return &v3Backend{cli: clientv3.NewKV(c), c: c}, nil
}

func (b *v3Backend) proto() string { return "v3" }

const dirMarker = ".dir"

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
	return strings.TrimRight(p, "/")
}

func withTrail(p string) string {
	p = normPath(p)
	if p == "/" {
		return "/"
	}
	return p + "/"
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
		rest := strings.TrimPrefix(key, prefix)
		rest = strings.TrimLeft(rest, "/")
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
		} else {
			ci.hasFile = true
			ci.fileValue = string(kv.Value)
		}
	}

	names := make([]string, 0, len(children))
	for n := range children {
		names = append(names, n)
	}
	sort.Strings(names)

	var nodes []*Node
	root := strings.TrimSuffix(prefix, "/")
	for _, name := range names {
		ci := children[name]
		full := root + "/" + name
		if ci.isDir {
			nodes = append(nodes, &Node{Name: full, IsDir: true, ClusterId: clusterID})
		}
		if ci.hasFile {
			nodes = append(nodes, &Node{Name: full, IsDir: false, Value: ci.fileValue, ClusterId: clusterID})
		}
	}
	return nodes, nil
}

func (b *v3Backend) get(key string) (*Node, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	k := normPath(key)

	exact, err := b.cli.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	if exact.Count > 0 {
		kv := exact.Kvs[0]
		return &Node{
			Name:      k,
			IsDir:     false,
			Value:     string(kv.Value),
			ClusterId: fmt.Sprintf("%d", exact.Header.GetClusterId()),
		}, nil
	}

	pfx := withTrail(k)
	dirProbe, err := b.cli.Get(ctx, pfx, clientv3.WithPrefix(), clientv3.WithLimit(1))
	if err != nil {
		return nil, err
	}
	if dirProbe.Count > 0 {
		return &Node{
			Name:      k,
			IsDir:     true,
			ClusterId: fmt.Sprintf("%d", dirProbe.Header.GetClusterId()),
		}, nil
	}

	return nil, fmt.Errorf("not found: %s", k)
}

func (b *v3Backend) set(key, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := b.cli.Put(ctx, normPath(key), value)
	return err
}

func (b *v3Backend) mkdir(directory string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := normPath(directory)
	_, err := b.cli.Put(ctx, dir+"/"+dirMarker, "")
	return err
}

func (b *v3Backend) del(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := b.cli.Delete(ctx, normPath(key))
	return err
}

func (b *v3Backend) deldir(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := b.cli.Delete(ctx, withTrail(key), clientv3.WithPrefix())
	return err
}

func (b *v3Backend) renameDir(oldDir, newDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	oldPfx := withTrail(oldDir)
	newPfx := withTrail(newDir)

	resp, err := b.cli.Get(ctx, oldPfx, clientv3.WithPrefix())
	if err != nil {
		return err
	}

	for _, kv := range resp.Kvs {
		newKey := strings.Replace(string(kv.Key), oldPfx, newPfx, 1)
		if _, err := b.cli.Put(ctx, newKey, string(kv.Value)); err != nil {
			return err
		}
	}
	_, err = b.cli.Delete(ctx, oldPfx, clientv3.WithPrefix())
	return err
}

type v2Backend struct {
	api    clientv2.KeysAPI
	client clientv2.Client
}

func newV2Backend(host, port, username, password string) (*v2Backend, error) {
	cfg := clientv2.Config{
		Endpoints: []string{fmt.Sprintf("http://%s:%s", host, port)},
	}
	if username != "" {
		cfg.Username = username
		cfg.Password = password
	}

	cli, err := clientv2.New(cfg)
	if err != nil {
		return nil, err
	}
	return &v2Backend{api: clientv2.NewKeysAPI(cli), client: cli}, nil
}

func (b *v2Backend) proto() string { return "v2" }

func (b *v2Backend) ls(directory string) ([]*Node, error) {
	resp, err := b.api.Get(context.Background(), directory,
		&clientv2.GetOptions{Sort: true, Recursive: false})
	if err != nil {
		if clientv2.IsKeyNotFound(err) {
			return []*Node{}, nil
		}
		return nil, err
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
	resp, err := b.api.Get(context.Background(), normPath(key), nil)
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
	_, err := b.api.Set(context.Background(), normPath(key), value, nil)
	return err
}

func (b *v2Backend) mkdir(directory string) error {
	_, err := b.api.Set(context.Background(), normPath(directory), "",
		&clientv2.SetOptions{Dir: true, PrevExist: clientv2.PrevIgnore})
	return err
}

func (b *v2Backend) del(key string) error {
	_, err := b.api.Delete(context.Background(), normPath(key), nil)
	return err
}

func (b *v2Backend) deldir(key string) error {
	_, err := b.api.Delete(context.Background(), normPath(key),
		&clientv2.DeleteOptions{Dir: true, Recursive: true})
	return err
}

func (b *v2Backend) renameDir(oldDir, newDir string) error {
	resp, err := b.api.Get(context.Background(), oldDir,
		&clientv2.GetOptions{Recursive: true})
	if err != nil {
		return err
	}

	for _, n := range resp.Node.Nodes {
		newKey := strings.Replace(n.Key, oldDir, newDir, 1)
		if n.Dir {
			_, err = b.api.Set(context.Background(), newKey, "",
				&clientv2.SetOptions{Dir: true, PrevExist: clientv2.PrevIgnore})
		} else {
			_, err = b.api.Set(context.Background(), newKey, n.Value, nil)
		}
		if err != nil {
			return err
		}
	}

	_, err = b.api.Delete(context.Background(), oldDir,
		&clientv2.DeleteOptions{Dir: true, Recursive: true})
	return err
}
