package main

import (
	"flag"
	"os"
	"strconv"

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
func (f *boolFlag) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	f.value = v
	f.set = true
	return nil
}

func main() {
	var (
		hostFlag          = &stringFlag{value: ""}
		portFlag          = &stringFlag{value: ""}
		protocolFlag      = &stringFlag{value: ""}
		debugFlag         = &boolFlag{value: false}
		usernameFlag      = &stringFlag{value: ""}
		passwordFlag      = &stringFlag{value: ""}
		tlsFlag           = &boolFlag{value: false}
		tlsCAFlag         = &stringFlag{value: ""}
		tlsCertFlag       = &stringFlag{value: ""}
		tlsKeyFlag        = &stringFlag{value: ""}
		tlsSkipVerifyFlag = &boolFlag{value: false}
		timeoutFlag       = &stringFlag{value: ""}
		configPath        = flag.String("config", config.DefaultPath, "config file, optional")
	)

	flag.Var(hostFlag, "host", "etcd host (e.g. 127.0.0.1)")
	flag.Var(portFlag, "port", "etcd port (e.g. 2379)")
	flag.Var(protocolFlag, "protocol", "etcd protocol: v2, v3, auto (default: auto)")
	flag.Var(debugFlag, "debug", "enable debug logging (true/false)")
	flag.Var(usernameFlag, "username", "etcd auth username")
	flag.Var(passwordFlag, "password", "etcd auth password (consider using config file)")
	flag.Var(tlsFlag, "tls", "enable TLS/HTTPS for etcd v3 (true/false)")
	flag.Var(tlsCAFlag, "tls-ca", "path to CA certificate file for TLS")
	flag.Var(tlsCertFlag, "tls-cert", "path to client certificate file for mutual TLS")
	flag.Var(tlsKeyFlag, "tls-key", "path to client key file for mutual TLS")
	flag.Var(tlsSkipVerifyFlag, "tls-skip-verify", "skip TLS server certificate verification (insecure)")
	flag.Var(timeoutFlag, "timeout", "etcd operation timeout in seconds (default: 5)")
	flag.Parse()

	// Hardcoded defaults
	host := "127.0.0.1"
	port := "2379"
	protocol := "auto"
	username := ""
	password := ""
	debug := false
	tlsEnabled := false
	tlsCAFile := ""
	tlsCertFile := ""
	tlsKeyFile := ""
	tlsSkipVerify := false
	timeoutSeconds := 0

	// Always load config first as a base; CLI flags override individual fields.
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.WithError(err).Warn("failed to load config, falling back to defaults")
	}
	if cfg != nil {
		if cfg.Host != "" {
			host = cfg.Host
		}
		if cfg.Port != "" {
			port = cfg.Port
		}
		if cfg.Protocol != "" {
			protocol = cfg.Protocol
		}
		if cfg.Username != "" {
			username = cfg.Username
		}
		if cfg.Password != "" {
			password = cfg.Password
		}
		debug = cfg.Debug
		tlsEnabled = cfg.TLSEnabled
		tlsCAFile = cfg.TLSCAFile
		tlsCertFile = cfg.TLSCertFile
		tlsKeyFile = cfg.TLSKeyFile
		tlsSkipVerify = cfg.TLSSkipVerify
		timeoutSeconds = cfg.TimeoutSeconds
	}

	// CLI flags take precedence over config file values.
	if hostFlag.set && hostFlag.value != "" {
		host = hostFlag.value
	}
	if portFlag.set && portFlag.value != "" {
		port = portFlag.value
	}
	if protocolFlag.set && protocolFlag.value != "" {
		protocol = protocolFlag.value
	}
	if usernameFlag.set {
		username = usernameFlag.value
	}
	if passwordFlag.set {
		password = passwordFlag.value
	}
	if debugFlag.set {
		debug = debugFlag.value
	}
	if tlsFlag.set {
		tlsEnabled = tlsFlag.value
	}
	if tlsCAFlag.set {
		tlsCAFile = tlsCAFlag.value
	}
	if tlsCertFlag.set {
		tlsCertFile = tlsCertFlag.value
	}
	if tlsKeyFlag.set {
		tlsKeyFile = tlsKeyFlag.value
	}
	if tlsSkipVerifyFlag.set {
		tlsSkipVerify = tlsSkipVerifyFlag.value
	}
	if timeoutFlag.set && timeoutFlag.value != "" {
		if v, err := strconv.Atoi(timeoutFlag.value); err == nil && v > 0 {
			timeoutSeconds = v
		}
	}
	log.SetOutput(os.Stderr)

	if debug {
		log.SetLevel(log.DebugLevel)
	}

	log.WithFields(log.Fields{
		"host":        host,
		"port":        port,
		"protocol":    protocol,
		"debug":       debug,
		"tls":         tlsEnabled,
		"timeout_sec": timeoutSeconds,
		"config":      *configPath,
	}).Debug("Starting etcd-walker")

	opts := model.Options{
		Host:           host,
		Port:           port,
		Protocol:       protocol,
		Username:       username,
		Password:       password,
		TLSEnabled:     tlsEnabled,
		TLSCAFile:      tlsCAFile,
		TLSCertFile:    tlsCertFile,
		TLSKeyFile:     tlsKeyFile,
		TLSSkipVerify:  tlsSkipVerify,
		TimeoutSeconds: timeoutSeconds,
	}
	ctrl := controller.NewController(opts, debug)
	if err := ctrl.Run(); err != nil {
		log.WithError(err).Error("etcd-walker exited with error")
		os.Exit(1)
	}
}
