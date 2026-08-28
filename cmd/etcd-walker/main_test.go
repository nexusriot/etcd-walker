package main

import (
	"flag"
	"io"
	"reflect"
	"strconv"
	"testing"

	"github.com/nexusriot/etcd-walker/pkg/config"
)

// stringFlag/boolFlag track whether a flag was given on the command line, so
// "flag explicitly set to the default value" still overrides the config file.

func TestStringFlagTracksSet(t *testing.T) {
	var f stringFlag
	if f.set {
		t.Error("fresh flag must not be marked set")
	}
	if err := f.Set("host1"); err != nil {
		t.Fatal(err)
	}
	if !f.set || f.value != "host1" || f.String() != "host1" {
		t.Errorf("after Set: %+v", f)
	}

	// Explicit empty string still counts as set.
	var empty stringFlag
	if err := empty.Set(""); err != nil {
		t.Fatal(err)
	}
	if !empty.set {
		t.Error("explicit empty value must be marked set")
	}
}

func TestBoolFlagParsesAndTracksSet(t *testing.T) {
	cases := map[string]bool{
		"true":  true,
		"1":     true,
		"false": false,
		"0":     false,
	}
	for in, want := range cases {
		var f boolFlag
		if err := f.Set(in); err != nil {
			t.Errorf("Set(%q) errored: %v", in, err)
			continue
		}
		if !f.set || f.value != want {
			t.Errorf("Set(%q) = %+v, want value=%t set=true", in, f, want)
		}
	}

	var f boolFlag
	if err := f.Set("banana"); err == nil {
		t.Error("invalid bool must error")
	}
	if f.set {
		t.Error("failed parse must not mark the flag as set")
	}
	if f.String() != "false" {
		t.Errorf("zero boolFlag String() = %q, want false", f.String())
	}
}

func TestSplitPrefixes(t *testing.T) {
	cases := map[string][]string{
		"/registry": {"/registry"},
		"/a,/b":     {"/a", "/b"},
		" /a , /b ": {"/a", "/b"},
		"/a,,/b,":   {"/a", "/b"}, // empties dropped, not turned into prefixes
		"":          nil,
		"   ":       nil,
		",":         nil,
	}
	for in, want := range cases {
		got := splitPrefixes(in)
		if len(got) != len(want) {
			t.Errorf("splitPrefixes(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("splitPrefixes(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}

// noFlags builds the same bundle main() runs, with nothing given on the
// command line. It goes through registerFlags so a field main() would forget
// cannot be silently supplied here instead.
func noFlags() cliFlags {
	return registerFlags(flag.NewFlagSet("test", flag.ContinueOnError))
}

// parseArgs registers the real flag set and parses argv through it — the whole
// startup path bar config loading.
func parseArgs(t *testing.T, args ...string) cliFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := registerFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	return f
}

func setStr(f *stringFlag, v string) *stringFlag { _ = f.Set(v); return f }
func setBool(f *boolFlag, v bool) *boolFlag      { _ = f.Set(strconv.FormatBool(v)); return f }

// With no config and no flags the hard-coded defaults stand.
func TestResolveDefaults(t *testing.T) {
	opts, policy, sess := resolve(nil, noFlags())

	if opts.Host != "127.0.0.1" || opts.Port != "2379" || opts.Protocol != "auto" {
		t.Errorf("defaults = %+v", opts)
	}
	if sess.debug || policy.ReadOnly || policy.DryRun || len(policy.ProtectedPrefixes) != 0 {
		t.Errorf("defaults should be permissive: debug=%v policy=%+v", sess.debug, policy)
	}
}

// Every config field must land in the right place — this is the copy-paste
// surface the extraction exists to protect.
func TestResolveAppliesEveryConfigField(t *testing.T) {
	cfg := &config.Config{
		Host: "h", Port: "1", Protocol: "v3", Debug: true,
		Username: "u", Password: "p",
		TLSEnabled: true, TLSCAFile: "ca", TLSCertFile: "crt",
		TLSKeyFile: "key", TLSSkipVerify: true, TimeoutSeconds: 9,
		ReadOnly: true, DryRun: true, ProtectedPrefixes: []string{"/a"},
	}
	opts, policy, sess := resolve(cfg, noFlags())

	if opts.Host != "h" || opts.Port != "1" || opts.Protocol != "v3" {
		t.Errorf("endpoint = %+v", opts)
	}
	if opts.Username != "u" || opts.Password != "p" {
		t.Errorf("credentials = %q/%q", opts.Username, opts.Password)
	}
	if !opts.TLSEnabled || opts.TLSCAFile != "ca" || opts.TLSCertFile != "crt" ||
		opts.TLSKeyFile != "key" || !opts.TLSSkipVerify {
		t.Errorf("TLS fields = %+v", opts)
	}
	if opts.TimeoutSeconds != 9 {
		t.Errorf("timeout = %d, want 9", opts.TimeoutSeconds)
	}
	if !sess.debug {
		t.Error("debug not applied")
	}
	if !policy.ReadOnly || !policy.DryRun || len(policy.ProtectedPrefixes) != 1 {
		t.Errorf("policy = %+v", policy)
	}
}

// A flag that was explicitly given always wins over the config file.
func TestResolveFlagsOverrideConfig(t *testing.T) {
	cfg := &config.Config{
		Host: "cfg-host", Port: "1111", Protocol: "v2", Debug: true,
		Username: "cfg-u", Password: "cfg-p", TLSEnabled: true,
		TimeoutSeconds: 3, ReadOnly: true, DryRun: true,
		ProtectedPrefixes: []string{"/from-config"},
	}
	f := noFlags()
	f.host = setStr(f.host, "flag-host")
	f.port = setStr(f.port, "2222")
	f.protocol = setStr(f.protocol, "v3")
	f.username = setStr(f.username, "flag-u")
	f.timeout = setStr(f.timeout, "42")
	f.protect = setStr(f.protect, "/one,/two")
	f.debug = setBool(f.debug, false)
	f.tls = setBool(f.tls, false)
	f.readOnly = setBool(f.readOnly, false)
	f.dryRun = setBool(f.dryRun, false)

	opts, policy, sess := resolve(cfg, f)

	if opts.Host != "flag-host" || opts.Port != "2222" || opts.Protocol != "v3" {
		t.Errorf("flags did not override endpoint: %+v", opts)
	}
	if opts.Username != "flag-u" {
		t.Errorf("username = %q, want flag-u", opts.Username)
	}
	// Untouched flag: the config value survives.
	if opts.Password != "cfg-p" {
		t.Errorf("password = %q, want the config value", opts.Password)
	}
	if opts.TimeoutSeconds != 42 {
		t.Errorf("timeout = %d, want 42", opts.TimeoutSeconds)
	}
	// Explicit false must be able to turn a config-enabled switch back OFF —
	// the whole reason the flag wrappers track "set".
	if sess.debug || opts.TLSEnabled || policy.ReadOnly || policy.DryRun {
		t.Errorf("explicit false did not override config: debug=%v tls=%v policy=%+v",
			sess.debug, opts.TLSEnabled, policy)
	}
	if len(policy.ProtectedPrefixes) != 2 || policy.ProtectedPrefixes[0] != "/one" {
		t.Errorf("-protect should REPLACE the configured list, got %v", policy.ProtectedPrefixes)
	}
}

// An empty -host must not blank out a configured host; an empty -username may,
// because clearing a credential is a legitimate intent.
func TestResolveEmptyFlagSemantics(t *testing.T) {
	cfg := &config.Config{Host: "cfg-host", Username: "cfg-u"}
	f := noFlags()
	f.host = setStr(f.host, "")
	f.username = setStr(f.username, "")

	opts, _, _ := resolve(cfg, f)

	if opts.Host != "cfg-host" {
		t.Errorf("empty -host blanked the configured host: %q", opts.Host)
	}
	if opts.Username != "" {
		t.Errorf("empty -username should clear the credential, got %q", opts.Username)
	}
}

// A non-numeric or non-positive -timeout is ignored rather than producing a
// zero/negative deadline.
func TestResolveRejectsBadTimeout(t *testing.T) {
	for _, bad := range []string{"abc", "0", "-5"} {
		f := noFlags()
		f.timeout = setStr(f.timeout, bad)
		opts, _, _ := resolve(&config.Config{TimeoutSeconds: 7}, f)
		if opts.TimeoutSeconds != 7 {
			t.Errorf("-timeout %q changed the timeout to %d, want the config's 7", bad, opts.TimeoutSeconds)
		}
	}
}

// B-17: registration and bundling are one function, so every flag the tool
// documents is reachable and no field can be left nil. A nil field used to
// compile, pass the suite, and panic on startup.
func TestRegisterFlagsWiresEveryField(t *testing.T) {
	f := noFlags()

	v := reflect.ValueOf(f)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsNil() {
			t.Errorf("cliFlags.%s is nil — resolve() would panic on startup",
				v.Type().Field(i).Name)
		}
	}
}

// Every flag the README documents must actually be registered.
func TestRegisterFlagsDeclaresDocumentedFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	registerFlags(fs)

	declared := map[string]bool{}
	fs.VisitAll(func(fl *flag.Flag) { declared[fl.Name] = true })

	for _, name := range []string{
		"host", "port", "protocol", "debug", "username", "password",
		"tls", "tls-ca", "tls-cert", "tls-key", "tls-skip-verify",
		"timeout", "read-only", "dry-run", "protect", "config",
		"no-snapshot", "dual",
	} {
		if !declared[name] {
			t.Errorf("-%s is documented but not registered", name)
		}
	}
}

// B-18: a boolean flag must accept the bare form the README uses. Without
// IsBoolFlag, `etcd-walker -read-only` fails with "flag needs an argument" —
// and that is the exact command the README's production example gives.
func TestBoolFlagsAcceptBareForm(t *testing.T) {
	for _, name := range []string{"read-only", "dry-run", "tls", "debug", "tls-skip-verify", "no-snapshot", "dual"} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		registerFlags(fs)
		if err := fs.Parse([]string{"-" + name}); err != nil {
			t.Errorf("-%s (bare) rejected: %v", name, err)
		}
	}
}

// …while the explicit form still works, which is what lets a flag turn a
// config-file setting back OFF.
func TestBoolFlagsAcceptExplicitValue(t *testing.T) {
	f := parseArgs(t, "-read-only=false", "-dry-run=true")

	if !f.readOnly.set || f.readOnly.value {
		t.Errorf("-read-only=false -> set=%v value=%v, want set/false", f.readOnly.set, f.readOnly.value)
	}
	if !f.dryRun.set || !f.dryRun.value {
		t.Errorf("-dry-run=true -> set=%v value=%v, want set/true", f.dryRun.set, f.dryRun.value)
	}
}

// End-to-end through the real flag set: argv beats the config file.
func TestParsedArgsOverrideConfig(t *testing.T) {
	cfg := &config.Config{Host: "cfg", ReadOnly: false, ProtectedPrefixes: []string{"/cfg"}}
	f := parseArgs(t, "-host", "argv", "-read-only", "-protect", "/a,/b")

	opts, policy, _ := resolve(cfg, f)

	if opts.Host != "argv" {
		t.Errorf("host = %q, want argv", opts.Host)
	}
	if !policy.ReadOnly {
		t.Error("bare -read-only did not enable read-only")
	}
	if len(policy.ProtectedPrefixes) != 2 {
		t.Errorf("protected = %v, want two entries from argv", policy.ProtectedPrefixes)
	}
}

// An unparsed bundle (nothing on the command line) must leave the config
// untouched — the regression that would appear if registerFlags pre-marked
// anything as "set".
func TestUnparsedFlagsLeaveConfigIntact(t *testing.T) {
	cfg := &config.Config{Host: "cfg-host", ReadOnly: true, DryRun: true, TimeoutSeconds: 11}
	opts, policy, _ := resolve(cfg, noFlags())

	if opts.Host != "cfg-host" || opts.TimeoutSeconds != 11 {
		t.Errorf("config not preserved: %+v", opts)
	}
	if !policy.ReadOnly || !policy.DryRun {
		t.Errorf("policy not preserved: %+v", policy)
	}
}

// Undo snapshots are on unless something turns them off, so an absent config
// key and an absent flag must both leave the protection in place. The config
// field is a pointer for exactly this reason: a plain bool's zero value would
// silently disable it for everyone who never wrote the key.
func TestResolveSnapshotDefaultsOn(t *testing.T) {
	if _, policy, _ := resolve(nil, noFlags()); !policy.SnapshotBeforeDelete {
		t.Error("snapshots must default to on with no config and no flags")
	}
	if _, policy, _ := resolve(&config.Config{Host: "h"}, noFlags()); !policy.SnapshotBeforeDelete {
		t.Error("a config that never mentions snapshots must leave them on")
	}

	off := false
	if _, policy, _ := resolve(&config.Config{SnapshotBeforeDelete: &off}, noFlags()); policy.SnapshotBeforeDelete {
		t.Error(`"snapshot_before_delete": false must turn them off`)
	}

	f := noFlags()
	f.noSnapshot = setBool(f.noSnapshot, true)
	if _, policy, _ := resolve(nil, f); policy.SnapshotBeforeDelete {
		t.Error("-no-snapshot must turn them off")
	}

	// -no-snapshot=false turns them back on over a config that disabled them.
	f = noFlags()
	f.noSnapshot = setBool(f.noSnapshot, false)
	if _, policy, _ := resolve(&config.Config{SnapshotBeforeDelete: &off}, f); !policy.SnapshotBeforeDelete {
		t.Error("-no-snapshot=false must re-enable snapshots over the config")
	}
}

func TestResolveDualPane(t *testing.T) {
	if _, _, sess := resolve(nil, noFlags()); sess.dual {
		t.Error("dual pane must default to off")
	}
	if _, _, sess := resolve(&config.Config{DualPane: true}, noFlags()); !sess.dual {
		t.Error(`"dual_pane": true not applied`)
	}
	f := noFlags()
	f.dual = setBool(f.dual, false)
	if _, _, sess := resolve(&config.Config{DualPane: true}, f); sess.dual {
		t.Error("-dual=false must override the config")
	}
}
