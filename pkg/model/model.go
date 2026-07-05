package model

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
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
	backend   backend
	authLabel string
}

type Node struct {
	Name      string
	ClusterId string
	IsDir     bool
	Value     string
	// TTL is the remaining time-to-live in seconds for the key. 0 means the
	// key has no expiry (no lease attached in v3 / no TTL set in v2).
	TTL int64
	// LeaseID is the v3 lease currently attached to the key, or 0 when the key
	// has no lease. It lets a value edit re-attach the same lease (preserving
	// the running expiry) and lets setTTL revoke the superseded lease instead
	// of orphaning it. Always 0 for v2, which has native per-key TTL.
	LeaseID int64
	// CreateRev/ModRev are the etcd revisions at which the key was created and
	// last modified (v3), or the created/modified indexes (v2). Version is the
	// v3 per-key modification counter; always 0 for v2. All 0 when unknown
	// (e.g. directory nodes).
	CreateRev int64
	ModRev    int64
	Version   int64
}

type Options struct {
	Host     string
	Port     string
	Protocol string // v2, v3, auto
	Username string
	Password string

	// TLS (v3 only)
	TLSEnabled    bool
	TLSCAFile     string
	TLSCertFile   string
	TLSKeyFile    string
	TLSSkipVerify bool

	// TimeoutSeconds for etcd operations; 0 defaults to 5s
	TimeoutSeconds int
}

func (m *Model) ProtocolVersion() string { return m.backend.proto() }
func (m *Model) AuthLabel() string {
	if m == nil || m.authLabel == "" {
		return "?"
	}
	return m.authLabel
}

func (m *Model) Ls(directory string) ([]*Node, error) { return m.backend.ls(directory) }
func (m *Model) Get(key string) (*Node, error)        { return m.backend.get(key) }
func (m *Model) Set(key, value string) error          { return m.backend.set(key, value) }

// SetTTL writes key=value with a time-to-live of ttlSeconds. A ttlSeconds <= 0
// clears any existing expiry (the key becomes permanent).
func (m *Model) SetTTL(key, value string, ttlSeconds int64) error {
	return m.backend.setTTL(key, value, ttlSeconds)
}

// SetKeepTTL writes key=value while preserving the key's existing expiry,
// instead of dropping it the way a plain Set does. On v3 it re-attaches
// leaseID (0 = write the key without a lease); on v2 it re-applies ttlSeconds
// (0 = no expiry). This is what the value editor uses so editing a key no
// longer silently clears its TTL.
func (m *Model) SetKeepTTL(key, value string, leaseID, ttlSeconds int64) error {
	return m.backend.setKeep(key, value, leaseID, ttlSeconds)
}
func (m *Model) MkDir(directory string) error                 { return m.backend.mkdir(directory) }
func (m *Model) Del(key string) error                         { return m.backend.del(key) }
func (m *Model) DelDir(key string) error                      { return m.backend.deldir(key) }
func (m *Model) RenameDir(oldDir, newDir string) error        { return m.backend.renameDir(oldDir, newDir) }
func (m *Model) RenameKey(oldKey, newKey string) error        { return m.backend.renameKey(oldKey, newKey) }
func (m *Model) Export(dir string) (map[string]string, error) { return m.backend.export(dir) }

// CopyKey duplicates a single key (value plus lease/TTL) to a new path,
// leaving the source untouched.
func (m *Model) CopyKey(src, dst string) error { return m.backend.copyKey(src, dst) }

// CopyDir duplicates a whole subtree (values plus leases/TTLs) under a new
// prefix, leaving the source untouched. Overlapping source/target is refused.
func (m *Model) CopyDir(srcDir, dstDir string) error { return m.backend.copyDir(srcDir, dstDir) }

// Search scans every key under dir for a case-insensitive substring match on
// the full key path — and on the value too when inValues is set. It returns
// at most limit nodes plus whether the result set was truncated at that cap.
func (m *Model) Search(dir, query string, inValues bool, limit int) ([]*Node, bool, error) {
	return m.backend.search(dir, query, inValues, limit)
}

// Import writes the given key/value pairs. Keys are normalized to absolute
// paths. When overwrite is false, keys that already exist as a value are
// skipped (existing directories never block a write). It returns how many
// keys were written and skipped; on the first write error it returns early
// with the counts accumulated so far.
func (m *Model) Import(items map[string]string, overwrite bool) (written, skipped int, err error) {
	for rawKey, value := range items {
		key := normPath(rawKey)
		if key == "/" {
			skipped++
			continue
		}
		if !overwrite {
			if n, gerr := m.backend.get(key); gerr == nil && n != nil && !n.IsDir {
				skipped++
				continue
			}
		}
		if serr := m.backend.set(key, value); serr != nil {
			return written, skipped, fmt.Errorf("import %s: %w", key, serr)
		}
		written++
	}
	return written, skipped, nil
}

type backend interface {
	proto() string
	probe() error
	ls(directory string) ([]*Node, error)
	get(key string) (*Node, error)
	set(key, value string) error
	setTTL(key, value string, ttlSeconds int64) error
	setKeep(key, value string, leaseID, ttlSeconds int64) error
	mkdir(directory string) error
	del(key string) error
	deldir(key string) error
	renameDir(oldDir, newDir string) error
	renameKey(oldKey, newKey string) error
	copyKey(src, dst string) error
	copyDir(srcDir, dstDir string) error
	search(dir, query string, inValues bool, limit int) ([]*Node, bool, error)
	authStatus() (enabled bool, known bool, err error)
	export(dir string) (map[string]string, error)
}

func NewModel(opts Options) (*Model, error) {
	host, port := opts.Host, opts.Port

	if strings.TrimSpace(opts.Username) == "" && strings.TrimSpace(opts.Password) != "" {
		return nil, fmt.Errorf("auth misconfigured: password is set but username is empty (set --username or username in config)")
	}
	switch strings.ToLower(strings.TrimSpace(opts.Protocol)) {

	case "v3":
		b3, err := newV3Backend(opts)
		if err != nil {
			return nil, fmt.Errorf("v3 init failed: %w", err)
		}
		if err := b3.probe(); err != nil {
			return nil, fmt.Errorf("v3 probe failed: %w", err)
		}

		label := "?"
		if en, known, _ := b3.authStatus(); known {
			if en {
				label = "ON"
			} else {
				label = "OFF"
			}
		}

		return &Model{backend: b3, authLabel: label}, nil

	case "auto":
		if b3, err := newV3Backend(opts); err == nil {
			if err := b3.probe(); err == nil {
				return &Model{backend: b3}, nil
			} else if isAuthRequiredErr(err) {
				return nil, fmt.Errorf("etcd auth is enabled; provide --username/--password (or set them in config). Original: %w", err)
			}
		}
		if b2, err := newV2Backend(opts); err == nil {
			if err := b2.probe(); err == nil {
				return &Model{backend: b2}, nil
			} else if isAuthRequiredErr(err) {
				return nil, fmt.Errorf("etcd auth is enabled; provide --username/--password (or set them in config). Original: %w", err)
			}
		}

		return nil, fmt.Errorf("auto: neither v3 nor v2 reachable at %s:%s", host, port)

	default: // v2
		b2, err := newV2Backend(opts)
		if err != nil {
			return nil, fmt.Errorf("v2 init failed: %w", err)
		}
		if err := b2.probe(); err != nil {
			return nil, fmt.Errorf("v2 probe failed: %w", err)
		}
		return &Model{backend: b2}, nil
	}
}

type v3Backend struct {
	cli     clientv3.KV
	c       *clientv3.Client
	timeout time.Duration
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

func newV3Backend(opts Options) (*v3Backend, error) {
	timeout := time.Duration(opts.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	scheme := "http"
	var tlsCfg *tls.Config

	if opts.TLSEnabled {
		scheme = "https"
		// #nosec G402 — InsecureSkipVerify is an explicit opt-in via config
		tlsCfg = &tls.Config{InsecureSkipVerify: opts.TLSSkipVerify} //nolint:gosec

		if opts.TLSCAFile != "" {
			caCert, err := os.ReadFile(opts.TLSCAFile)
			if err != nil {
				return nil, fmt.Errorf("read TLS CA file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caCert) {
				return nil, fmt.Errorf("invalid TLS CA cert in %s", opts.TLSCAFile)
			}
			tlsCfg.RootCAs = pool
		}

		if opts.TLSCertFile != "" && opts.TLSKeyFile != "" {
			cert, err := tls.LoadX509KeyPair(opts.TLSCertFile, opts.TLSKeyFile)
			if err != nil {
				return nil, fmt.Errorf("load TLS client keypair: %w", err)
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
	}

	cfg := clientv3.Config{
		Endpoints:   []string{fmt.Sprintf("%s://%s:%s", scheme, opts.Host, opts.Port)},
		DialTimeout: timeout,
		Logger:      zap.NewNop(),
		TLS:         tlsCfg,
	}
	if opts.Username != "" {
		cfg.Username = opts.Username
		cfg.Password = opts.Password
	}

	c, err := clientv3.New(cfg)
	if err != nil {
		return nil, err
	}
	return &v3Backend{cli: clientv3.NewKV(c), c: c, timeout: timeout}, nil
}

func (b *v3Backend) proto() string { return "v3" }

// probe checks reachability (and surfaces auth errors) with a minimal
// keys-only, limit-1 request instead of pulling the whole keyspace the way
// ls("/") would.
func (b *v3Backend) probe() error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	_, err := b.cli.Get(ctx, "/", clientv3.WithPrefix(), clientv3.WithKeysOnly(), clientv3.WithLimit(1))
	return err
}

// ttlForLease returns the remaining seconds for a lease id, or 0 when the
// lease is absent/expired or cannot be resolved. b.c is nil in unit tests, so
// callers tolerate a 0 result.
func (b *v3Backend) ttlForLease(ctx context.Context, lease int64) int64 {
	if lease == 0 || b.c == nil {
		return 0
	}
	resp, err := b.c.TimeToLive(ctx, clientv3.LeaseID(lease))
	if err != nil || resp.TTL < 0 {
		return 0
	}
	return resp.TTL
}

const dirMarker = ".dir"

// IsReservedName reports whether a basename is reserved for internal
// bookkeeping (the v3 directory marker). Creating a key with this name would
// make it invisible in listings and turn its parent into a phantom directory.
func IsReservedName(name string) bool { return name == dirMarker }

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
	if p == "/" {
		return "/"
	}
	return strings.TrimRight(p, "/")
}

// underOrEqual reports whether b is the same directory as a or nested under a.
func underOrEqual(a, b string) bool {
	a, b = normPath(a), normPath(b)
	if a == b {
		return true
	}
	if a == "/" {
		return true
	}
	return strings.HasPrefix(b, a+"/")
}

func dirsOverlap(a, b string) bool { return underOrEqual(a, b) || underOrEqual(b, a) }

// renameDirGuard rejects directory renames whose source and target overlap.
// Both backends copy the subtree and then delete the source prefix; if the
// paths are nested, that delete would also wipe the freshly copied data.
func renameDirGuard(oldDir, newDir string) error {
	o, n := normPath(oldDir), normPath(newDir)
	if dirsOverlap(o, n) {
		return fmt.Errorf("cannot rename directory %s to %s: paths overlap (would cause data loss)", o, n)
	}
	return nil
}

// copyDirGuard rejects copies whose source and target overlap: the copy would
// land inside the tree being copied (or bury the target's own source),
// producing a tangled hierarchy nobody intends.
func copyDirGuard(srcDir, dstDir string) error {
	s, d := normPath(srcDir), normPath(dstDir)
	if dirsOverlap(s, d) {
		return fmt.Errorf("cannot copy directory %s to %s: paths overlap", s, d)
	}
	return nil
}

func withTrail(p string) string {
	p = normPath(p)
	if p == "/" {
		return "/"
	}
	return p + "/"
}

func (b *v3Backend) ls(directory string) ([]*Node, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()

	// Keys-only: a listing only needs names to build the tree level, and the
	// details pane re-fetches the focused key anyway. Transferring every value
	// under the prefix made listing large trees painfully heavy.
	prefix := withTrail(directory)
	resp, err := b.cli.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return nil, err
	}

	clusterID := fmt.Sprintf("%d", resp.Header.GetClusterId())

	type childInfo struct {
		isDir   bool
		hasFile bool
		lease   int64
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
			ci.lease = kv.Lease
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
			// Value is deliberately left empty (keys-only listing) — the
			// details pane re-reads the focused key, which is also where the
			// TTL countdown comes from. Carry the cheap lease id (still
			// present in keys-only responses) for the on-focus refresh.
			nodes = append(nodes, &Node{
				Name:      full,
				IsDir:     false,
				ClusterId: clusterID,
				LeaseID:   ci.lease,
			})
		}
	}
	return nodes, nil
}

func (b *v3Backend) get(key string) (*Node, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
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
			TTL:       b.ttlForLease(ctx, kv.Lease),
			LeaseID:   kv.Lease,
			CreateRev: kv.CreateRevision,
			ModRev:    kv.ModRevision,
			Version:   kv.Version,
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
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	_, err := b.cli.Put(ctx, normPath(key), value)
	return err
}

func (b *v3Backend) setTTL(key, value string, ttlSeconds int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	k := normPath(key)

	// Note the lease currently on the key (if any) so we can revoke it once the
	// key points at its replacement; otherwise every TTL change would orphan a
	// lease that keeps ticking on the server.
	var oldLease clientv3.LeaseID
	if cur, err := b.cli.Get(ctx, k); err == nil && cur.Count > 0 {
		oldLease = clientv3.LeaseID(cur.Kvs[0].Lease)
	}

	switch {
	case ttlSeconds <= 0:
		// Clearing the TTL: a plain Put detaches any previously attached lease.
		if _, err := b.cli.Put(ctx, k, value); err != nil {
			return err
		}
	default:
		if b.c == nil {
			return fmt.Errorf("lease operations require a live etcd client")
		}
		lease, err := b.c.Grant(ctx, ttlSeconds)
		if err != nil {
			return fmt.Errorf("grant lease: %w", err)
		}
		if _, err := b.cli.Put(ctx, k, value, clientv3.WithLease(lease.ID)); err != nil {
			return err
		}
	}

	// Best-effort cleanup of the superseded lease. The key no longer references
	// it (the Put above re-pointed it), so revoking only reaps the orphan. This
	// tool attaches one key per lease, so nothing else is collateral.
	if oldLease != 0 && b.c != nil {
		_, _ = b.c.Revoke(ctx, oldLease)
	}
	return nil
}

// setKeep updates the key's value while preserving its current lease, so an
// ordinary value edit no longer detaches the key's TTL. leaseID 0 means the
// key has no lease and is written permanently.
func (b *v3Backend) setKeep(key, value string, leaseID, _ int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	k := normPath(key)
	if leaseID == 0 {
		_, err := b.cli.Put(ctx, k, value)
		return err
	}
	_, err := b.cli.Put(ctx, k, value, clientv3.WithLease(clientv3.LeaseID(leaseID)))
	return err
}

func (b *v3Backend) mkdir(directory string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	dir := normPath(directory)
	_, err := b.cli.Put(ctx, dir+"/"+dirMarker, "")
	return err
}

func (b *v3Backend) del(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	_, err := b.cli.Delete(ctx, normPath(key))
	return err
}

func (b *v3Backend) deldir(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout*2)
	defer cancel()
	_, err := b.cli.Delete(ctx, withTrail(key), clientv3.WithPrefix())
	return err
}

// treeTimeout stretches the per-op timeout for whole-subtree operations
// (rename/copy of directories).
func (b *v3Backend) treeTimeout() time.Duration {
	timeout := b.timeout * 4
	if timeout < 20*time.Second {
		timeout = 20 * time.Second
	}
	return timeout
}

// copyPrefix writes every key under oldPfx to the same relative path under
// newPfx, re-attaching each key's lease so expiring keys stay expiring. Both
// prefixes must already carry their trailing slash.
func (b *v3Backend) copyPrefix(ctx context.Context, oldPfx, newPfx string) error {
	resp, err := b.cli.Get(ctx, oldPfx, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	for _, kv := range resp.Kvs {
		newKey := newPfx + strings.TrimPrefix(string(kv.Key), oldPfx)
		var opts []clientv3.OpOption
		if kv.Lease != 0 {
			opts = append(opts, clientv3.WithLease(clientv3.LeaseID(kv.Lease)))
		}
		if _, err := b.cli.Put(ctx, newKey, string(kv.Value), opts...); err != nil {
			return err
		}
	}
	return nil
}

func (b *v3Backend) renameDir(oldDir, newDir string) error {
	if err := renameDirGuard(oldDir, newDir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.treeTimeout())
	defer cancel()

	if err := b.copyPrefix(ctx, withTrail(oldDir), withTrail(newDir)); err != nil {
		return err
	}
	_, err := b.cli.Delete(ctx, withTrail(oldDir), clientv3.WithPrefix())
	return err
}

func (b *v3Backend) copyDir(srcDir, dstDir string) error {
	if err := copyDirGuard(srcDir, dstDir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.treeTimeout())
	defer cancel()
	return b.copyPrefix(ctx, withTrail(srcDir), withTrail(dstDir))
}

func (b *v3Backend) copyKey(src, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout*2)
	defer cancel()

	s, d := normPath(src), normPath(dst)
	if s == d {
		return fmt.Errorf("source and target are the same key: %s", s)
	}
	resp, err := b.cli.Get(ctx, s)
	if err != nil {
		return err
	}
	if resp.Count == 0 {
		return fmt.Errorf("key not found: %s", s)
	}
	kv := resp.Kvs[0]
	// Carry the lease along so the copy expires with the original.
	var opts []clientv3.OpOption
	if kv.Lease != 0 {
		opts = append(opts, clientv3.WithLease(clientv3.LeaseID(kv.Lease)))
	}
	_, err = b.cli.Put(ctx, d, string(kv.Value), opts...)
	return err
}

func (b *v3Backend) renameKey(oldKey, newKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout*2)
	defer cancel()

	old := normPath(oldKey)
	resp, err := b.cli.Get(ctx, old)
	if err != nil {
		return err
	}
	if resp.Count == 0 {
		return fmt.Errorf("key not found: %s", old)
	}
	kv := resp.Kvs[0]
	// Preserve the key's lease (and thus its remaining TTL) across the rename.
	var opts []clientv3.OpOption
	if kv.Lease != 0 {
		opts = append(opts, clientv3.WithLease(clientv3.LeaseID(kv.Lease)))
	}
	if _, err := b.cli.Put(ctx, normPath(newKey), string(kv.Value), opts...); err != nil {
		return err
	}
	_, err = b.cli.Delete(ctx, old)
	return err
}

func (b *v3Backend) export(dir string) (map[string]string, error) {
	timeout := b.timeout * 10
	if timeout < 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	prefix := withTrail(dir)
	resp, err := b.cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}

	result := make(map[string]string, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		key := string(kv.Key)
		// skip the synthetic directory marker key (.dir)
		if strings.HasSuffix(key, "/"+dirMarker) {
			continue
		}
		result[key] = string(kv.Value)
	}
	return result, nil
}

func (b *v3Backend) search(dir, query string, inValues bool, limit int) ([]*Node, bool, error) {
	timeout := b.timeout * 10
	if timeout < 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	prefix := withTrail(dir)
	opts := []clientv3.OpOption{clientv3.WithPrefix()}
	if !inValues {
		// Path-only search never looks at values; keep the sweep keys-only.
		opts = append(opts, clientv3.WithKeysOnly())
	}
	resp, err := b.cli.Get(ctx, prefix, opts...)
	if err != nil {
		return nil, false, err
	}

	clusterID := fmt.Sprintf("%d", resp.Header.GetClusterId())
	q := strings.ToLower(query)
	var out []*Node
	for _, kv := range resp.Kvs {
		key := string(kv.Key)
		if strings.HasSuffix(key, "/"+dirMarker) {
			continue
		}
		if !strings.Contains(strings.ToLower(key), q) &&
			!(inValues && strings.Contains(strings.ToLower(string(kv.Value)), q)) {
			continue
		}
		if len(out) >= limit {
			return out, true, nil
		}
		out = append(out, &Node{
			Name:      normPath(key),
			Value:     string(kv.Value),
			ClusterId: clusterID,
			LeaseID:   kv.Lease,
		})
	}
	return out, false, nil
}

func (b *v3Backend) authStatus() (bool, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resp, err := b.c.Auth.AuthStatus(ctx)
	if err == nil {
		return resp.Enabled, true, nil
	}

	s := err.Error()
	// If the server rejected the call due to auth, auth must be enabled.
	if strings.Contains(strings.ToLower(s), "permission denied") ||
		strings.Contains(strings.ToLower(s), "unauthorized") {
		return true, true, nil
	}
	return false, false, err
}

type v2Backend struct {
	api     clientv2.KeysAPI
	client  clientv2.Client
	timeout time.Duration
}

func newV2Backend(opts Options) (*v2Backend, error) {
	timeout := time.Duration(opts.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	cfg := clientv2.Config{
		Endpoints: []string{fmt.Sprintf("http://%s:%s", opts.Host, opts.Port)},
	}
	if opts.Username != "" {
		cfg.Username = opts.Username
		cfg.Password = opts.Password
	}

	cli, err := clientv2.New(cfg)
	if err != nil {
		return nil, err
	}
	return &v2Backend{api: clientv2.NewKeysAPI(cli), client: cli, timeout: timeout}, nil
}

func (b *v2Backend) proto() string { return "v2" }

// probe checks reachability with a non-recursive root listing, which v2
// serves cheaply (single level, no subtree walk).
func (b *v2Backend) probe() error {
	_, err := b.ls("/")
	return err
}

func (b *v2Backend) ls(directory string) ([]*Node, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	resp, err := b.api.Get(ctx, directory,
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
			TTL:       n.TTL,
		})
	}
	return nds, nil
}

func (b *v2Backend) get(key string) (*Node, error) {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	resp, err := b.api.Get(ctx, normPath(key), nil)
	if err != nil {
		return nil, err
	}
	return &Node{
		Name:      resp.Node.Key,
		ClusterId: resp.ClusterID,
		IsDir:     resp.Node.Dir,
		Value:     resp.Node.Value,
		TTL:       resp.Node.TTL,
		CreateRev: int64(resp.Node.CreatedIndex),
		ModRev:    int64(resp.Node.ModifiedIndex),
	}, nil
}

func (b *v2Backend) set(key, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	_, err := b.api.Set(ctx, normPath(key), value, nil)
	return err
}

func (b *v2Backend) setTTL(key, value string, ttlSeconds int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	opts := &clientv2.SetOptions{}
	if ttlSeconds > 0 {
		opts.TTL = time.Duration(ttlSeconds) * time.Second
	}
	_, err := b.api.Set(ctx, normPath(key), value, opts)
	return err
}

// setKeep updates the value while keeping the key's expiry by re-applying the
// remaining TTL (0 = permanent). v2 has no leases, so leaseID is ignored.
func (b *v2Backend) setKeep(key, value string, _, ttlSeconds int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	opts := &clientv2.SetOptions{}
	if ttlSeconds > 0 {
		opts.TTL = time.Duration(ttlSeconds) * time.Second
	}
	_, err := b.api.Set(ctx, normPath(key), value, opts)
	return err
}

func (b *v2Backend) mkdir(directory string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	_, err := b.api.Set(ctx, normPath(directory), "",
		&clientv2.SetOptions{Dir: true, PrevExist: clientv2.PrevIgnore})
	return err
}

func (b *v2Backend) del(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	_, err := b.api.Delete(ctx, normPath(key), nil)
	return err
}

func (b *v2Backend) deldir(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout*2)
	defer cancel()
	_, err := b.api.Delete(ctx, normPath(key),
		&clientv2.DeleteOptions{Dir: true, Recursive: true})
	return err
}

func (b *v2Backend) treeTimeout() time.Duration {
	timeout := b.timeout * 4
	if timeout < 20*time.Second {
		timeout = 20 * time.Second
	}
	return timeout
}

// copyTree replicates the whole subtree at srcDir under dstDir, TTLs
// included. It always creates dstDir explicitly first — without that an
// empty source directory would produce nothing at all (and, for rename,
// then vanish with the delete).
func (b *v2Backend) copyTree(ctx context.Context, srcDir, dstDir string) error {
	resp, err := b.api.Get(ctx, srcDir, &clientv2.GetOptions{Recursive: true})
	if err != nil {
		return err
	}

	mkOpts := &clientv2.SetOptions{Dir: true, PrevExist: clientv2.PrevIgnore}
	if resp.Node.TTL > 0 {
		mkOpts.TTL = time.Duration(resp.Node.TTL) * time.Second
	}
	if _, err := b.api.Set(ctx, dstDir, "", mkOpts); err != nil {
		return err
	}

	return b.v2CopyNodes(ctx, resp.Node.Nodes, srcDir, dstDir)
}

func (b *v2Backend) renameDir(oldDir, newDir string) error {
	if err := renameDirGuard(oldDir, newDir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.treeTimeout())
	defer cancel()

	if err := b.copyTree(ctx, oldDir, newDir); err != nil {
		return err
	}
	_, err := b.api.Delete(ctx, oldDir, &clientv2.DeleteOptions{Dir: true, Recursive: true})
	return err
}

func (b *v2Backend) copyDir(srcDir, dstDir string) error {
	if err := copyDirGuard(srcDir, dstDir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.treeTimeout())
	defer cancel()
	return b.copyTree(ctx, normPath(srcDir), normPath(dstDir))
}

func (b *v2Backend) copyKey(src, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout*2)
	defer cancel()

	s, d := normPath(src), normPath(dst)
	if s == d {
		return fmt.Errorf("source and target are the same key: %s", s)
	}
	resp, err := b.api.Get(ctx, s, nil)
	if err != nil {
		return err
	}
	// Carry the remaining TTL along so the copy expires with the original.
	var opts *clientv2.SetOptions
	if resp.Node.TTL > 0 {
		opts = &clientv2.SetOptions{TTL: time.Duration(resp.Node.TTL) * time.Second}
	}
	_, err = b.api.Set(ctx, d, resp.Node.Value, opts)
	return err
}

// v2CopyNodes recursively copies nodes from oldDir prefix to newDir prefix,
// re-applying each node's remaining TTL so a rename does not silently make
// expiring entries permanent.
func (b *v2Backend) v2CopyNodes(ctx context.Context, nodes clientv2.Nodes, oldDir, newDir string) error {
	for _, n := range nodes {
		newKey := newDir + strings.TrimPrefix(n.Key, oldDir)
		if n.Dir {
			opts := &clientv2.SetOptions{Dir: true, PrevExist: clientv2.PrevIgnore}
			if n.TTL > 0 {
				opts.TTL = time.Duration(n.TTL) * time.Second
			}
			if _, err := b.api.Set(ctx, newKey, "", opts); err != nil {
				return err
			}
			if err := b.v2CopyNodes(ctx, n.Nodes, oldDir, newDir); err != nil {
				return err
			}
		} else {
			var opts *clientv2.SetOptions
			if n.TTL > 0 {
				opts = &clientv2.SetOptions{TTL: time.Duration(n.TTL) * time.Second}
			}
			if _, err := b.api.Set(ctx, newKey, n.Value, opts); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *v2Backend) renameKey(oldKey, newKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout*2)
	defer cancel()
	resp, err := b.api.Get(ctx, normPath(oldKey), nil)
	if err != nil {
		return err
	}
	// Preserve the key's remaining TTL across the rename.
	var opts *clientv2.SetOptions
	if resp.Node.TTL > 0 {
		opts = &clientv2.SetOptions{TTL: time.Duration(resp.Node.TTL) * time.Second}
	}
	if _, err := b.api.Set(ctx, normPath(newKey), resp.Node.Value, opts); err != nil {
		return err
	}
	_, err = b.api.Delete(ctx, normPath(oldKey), nil)
	return err
}

func (b *v2Backend) export(dir string) (map[string]string, error) {
	timeout := b.timeout * 10
	if timeout < 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resp, err := b.api.Get(ctx, normPath(dir), &clientv2.GetOptions{Recursive: true})
	if err != nil {
		if clientv2.IsKeyNotFound(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	result := make(map[string]string)
	v2collectKeys(resp.Node.Nodes, result)
	return result, nil
}

func v2collectKeys(nodes clientv2.Nodes, result map[string]string) {
	for _, n := range nodes {
		if n.Dir {
			v2collectKeys(n.Nodes, result)
		} else {
			result[n.Key] = n.Value
		}
	}
}

func (b *v2Backend) search(dir, query string, inValues bool, limit int) ([]*Node, bool, error) {
	timeout := b.timeout * 10
	if timeout < 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resp, err := b.api.Get(ctx, normPath(dir), &clientv2.GetOptions{Recursive: true, Sort: true})
	if err != nil {
		if clientv2.IsKeyNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	q := strings.ToLower(query)
	var out []*Node
	truncated := false
	var walk func(nodes clientv2.Nodes) bool // false = stop, limit reached
	walk = func(nodes clientv2.Nodes) bool {
		for _, n := range nodes {
			if n.Dir {
				if !walk(n.Nodes) {
					return false
				}
				continue
			}
			if !strings.Contains(strings.ToLower(n.Key), q) &&
				!(inValues && strings.Contains(strings.ToLower(n.Value), q)) {
				continue
			}
			if len(out) >= limit {
				truncated = true
				return false
			}
			out = append(out, &Node{
				Name:      n.Key,
				Value:     n.Value,
				TTL:       n.TTL,
				ClusterId: resp.ClusterID,
			})
		}
		return true
	}
	walk(resp.Node.Nodes)
	return out, truncated, nil
}

func (b *v2Backend) authStatus() (enabled bool, known bool, err error) {
	cfg := clientv2.Config{
		Endpoints: b.client.Endpoints(),
	}

	cli, err := clientv2.New(cfg)
	if err != nil {
		return false, false, err
	}
	api := clientv2.NewKeysAPI(cli)

	timeout := b.timeout
	if timeout > 3*time.Second {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, err = api.Get(ctx, "/", nil)
	if err == nil {
		return false, true, nil
	}

	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "authentication"),
		strings.Contains(msg, "permission denied"),
		strings.Contains(msg, "401"):
		return true, true, nil

	case clientv2.IsKeyNotFound(err):
		return false, true, nil
	}

	return false, false, err
}
