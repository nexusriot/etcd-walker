package main

import (
	"flag"
	"os"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/etcd-walker/pkg/config"
	"github.com/nexusriot/etcd-walker/pkg/controller"
	"github.com/nexusriot/etcd-walker/pkg/model"
)

type stringFlag struct {
	value string
	set   bool
}

func (f *stringFlag) String() string { return f.value }
func (f *stringFlag) Set(s string) error {
	f.value = s
	f.set = true
	return nil
}

type boolFlag struct {
	value bool
	set   bool
}

func (f *boolFlag) String() string { return strconv.FormatBool(f.value) }

// IsBoolFlag makes the flag package accept the bare `-read-only` form, the way
// every other Go tool spells a boolean. Without it, flag.Var treats the value
// as requiring an argument and `-read-only` fails with "flag needs an
// argument" — which is exactly how the README documents it. `-read-only=false`
// keeps working, which is what makes "explicitly turn a config setting off"
// possible.
func (f *boolFlag) IsBoolFlag() bool { return true }

func (f *boolFlag) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	f.value = v
	f.set = true
	return nil
}

// splitPrefixes parses the comma-separated -protect value, dropping empties so
// a trailing comma or a bare "" does not become a prefix that matches nothing
// (or, worse, everything).
func splitPrefixes(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// cliFlags bundles every command-line flag so the precedence rules can be
// resolved (and tested) as one pure step.
type cliFlags struct {
	host, port, protocol      *stringFlag
	username, password        *stringFlag
	tlsCA, tlsCert, tlsKey    *stringFlag
	timeout, protect          *stringFlag
	debug, tls, tlsSkipVerify *boolFlag
	readOnly, dryRun          *boolFlag
	configPath                *string
}

// registerFlags declares every flag on fs and returns the bundle wired to it.
//
// Registration and bundling live in ONE function on purpose: when they were
// two lists — flag.Var calls in main() plus a struct literal handing them to
// resolve() — dropping a field from the literal still compiled, still passed
// the whole test suite, and nil-panicked on startup. Now there is nowhere to
// forget, and tests construct the same bundle main() runs.
func registerFlags(fs *flag.FlagSet) cliFlags {
	f := cliFlags{
		host: &stringFlag{}, port: &stringFlag{}, protocol: &stringFlag{},
		username: &stringFlag{}, password: &stringFlag{},
		tlsCA: &stringFlag{}, tlsCert: &stringFlag{}, tlsKey: &stringFlag{},
		timeout: &stringFlag{}, protect: &stringFlag{},
		debug: &boolFlag{}, tls: &boolFlag{}, tlsSkipVerify: &boolFlag{},
		readOnly: &boolFlag{}, dryRun: &boolFlag{},
	}
	fs.Var(f.host, "host", "etcd host (e.g. 127.0.0.1)")
	fs.Var(f.port, "port", "etcd port (e.g. 2379)")
	fs.Var(f.protocol, "protocol", "etcd protocol: v2, v3, auto (default: auto)")
	fs.Var(f.debug, "debug", "enable debug logging")
	fs.Var(f.username, "username", "etcd auth username")
	fs.Var(f.password, "password", "etcd auth password (consider using config file)")
	fs.Var(f.tls, "tls", "enable TLS/HTTPS for etcd v3")
	fs.Var(f.tlsCA, "tls-ca", "path to CA certificate file for TLS")
	fs.Var(f.tlsCert, "tls-cert", "path to client certificate file for mutual TLS")
	fs.Var(f.tlsKey, "tls-key", "path to client key file for mutual TLS")
	fs.Var(f.tlsSkipVerify, "tls-skip-verify", "skip TLS server certificate verification (insecure)")
	fs.Var(f.timeout, "timeout", "etcd operation timeout in seconds (default: 5)")
	fs.Var(f.readOnly, "read-only", "refuse every mutating action for this session")
	fs.Var(f.dryRun, "dry-run", "record what each change would do without performing it")
	fs.Var(f.protect, "protect", "comma-separated prefixes needing a typed confirmation before any write (e.g. /registry)")
	f.configPath = fs.String("config", config.DefaultPath, "config file, optional")
	return f
}

// resolve applies the three-tier precedence — hard-coded defaults, then the
// config file, then any flag explicitly given on the command line — and splits
// the result into the connection options and the session policy.
//
// It is separated from main() because this is the one part of startup with
// real logic: ~20 fields each needing "config unless the flag was set", where
// a single copy-paste puts a value in the wrong field and nothing complains.
func resolve(cfg *config.Config, f cliFlags) (model.Options, controller.Policy, bool) {
	opts := model.Options{
		Host:     "127.0.0.1",
		Port:     "2379",
		Protocol: "auto",
	}
	var policy controller.Policy
	debug := false

	if cfg != nil {
		if cfg.Host != "" {
			opts.Host = cfg.Host
		}
		if cfg.Port != "" {
			opts.Port = cfg.Port
		}
		if cfg.Protocol != "" {
			opts.Protocol = cfg.Protocol
		}
		if cfg.Username != "" {
			opts.Username = cfg.Username
		}
		if cfg.Password != "" {
			opts.Password = cfg.Password
		}
		debug = cfg.Debug
		opts.TLSEnabled = cfg.TLSEnabled
		opts.TLSCAFile = cfg.TLSCAFile
		opts.TLSCertFile = cfg.TLSCertFile
		opts.TLSKeyFile = cfg.TLSKeyFile
		opts.TLSSkipVerify = cfg.TLSSkipVerify
		opts.TimeoutSeconds = cfg.TimeoutSeconds
		policy.ReadOnly = cfg.ReadOnly
		policy.DryRun = cfg.DryRun
		policy.ProtectedPrefixes = cfg.ProtectedPrefixes
	}

	// CLI flags take precedence over config file values.
	if f.host.set && f.host.value != "" {
		opts.Host = f.host.value
	}
	if f.port.set && f.port.value != "" {
		opts.Port = f.port.value
	}
	if f.protocol.set && f.protocol.value != "" {
		opts.Protocol = f.protocol.value
	}
	if f.username.set {
		opts.Username = f.username.value
	}
	if f.password.set {
		opts.Password = f.password.value
	}
	if f.debug.set {
		debug = f.debug.value
	}
	if f.tls.set {
		opts.TLSEnabled = f.tls.value
	}
	if f.tlsCA.set {
		opts.TLSCAFile = f.tlsCA.value
	}
	if f.tlsCert.set {
		opts.TLSCertFile = f.tlsCert.value
	}
	if f.tlsKey.set {
		opts.TLSKeyFile = f.tlsKey.value
	}
	if f.tlsSkipVerify.set {
		opts.TLSSkipVerify = f.tlsSkipVerify.value
	}
	if f.timeout.set && f.timeout.value != "" {
		if v, err := strconv.Atoi(f.timeout.value); err == nil && v > 0 {
			opts.TimeoutSeconds = v
		}
	}
	if f.readOnly.set {
		policy.ReadOnly = f.readOnly.value
	}
	if f.dryRun.set {
		policy.DryRun = f.dryRun.value
	}
	// -protect replaces the configured list rather than adding to it, so a
	// session can always be reasoned about from its own command line.
	if f.protect.set {
		policy.ProtectedPrefixes = splitPrefixes(f.protect.value)
	}
	return opts, policy, debug
}

func main() {
	f := registerFlags(flag.CommandLine)
	flag.Parse()

	cfg, err := config.Load(*f.configPath)
	if err != nil {
		log.WithError(err).Warn("failed to load config, falling back to defaults")
	}
	opts, policy, debug := resolve(cfg, f)

	log.SetOutput(os.Stderr)

	if debug {
		log.SetLevel(log.DebugLevel)
	}

	log.WithFields(log.Fields{
		"host":        opts.Host,
		"port":        opts.Port,
		"protocol":    opts.Protocol,
		"debug":       debug,
		"tls":         opts.TLSEnabled,
		"timeout_sec": opts.TimeoutSeconds,
		"read_only":   policy.ReadOnly,
		"dry_run":     policy.DryRun,
		"protected":   len(policy.ProtectedPrefixes),
		"config":      *f.configPath,
	}).Debug("Starting etcd-walker")

	ctrl := controller.NewController(opts, policy)
	if err := ctrl.Run(); err != nil {
		log.WithError(err).Error("etcd-walker exited with error")
		os.Exit(1)
	}
}
