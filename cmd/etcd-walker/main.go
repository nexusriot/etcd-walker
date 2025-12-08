package main

import (
	"flag"
	"os"
	"strconv"

	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/etcd-walker/pkg/config"
	"github.com/nexusriot/etcd-walker/pkg/controller"
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
		hostFlag     = &stringFlag{value: ""}
		portFlag     = &stringFlag{value: ""}
		protocolFlag = &stringFlag{value: ""}
		debugFlag    = &boolFlag{value: false}
		configPath   = flag.String("config", config.DefaultPath, "config file, optional")
	)

	flag.Var(hostFlag, "host", "etcd host (e.g. 127.0.0.1)")
	flag.Var(portFlag, "port", "etcd port (e.g. 2379)")
	flag.Var(protocolFlag, "protocol", "etcd protocol: v2, v3, auto (default: auto)")
	flag.Var(debugFlag, "debug", "enable debug logging (true/false)")

	flag.Parse()

	// Hardcoded defaults
	host := "127.0.0.1"
	port := "2379"
	protocol := "auto"
	debug := false

	// Check whether any of the connection-related CLI flags are explicitly set.
	cliOverrides := hostFlag.set || portFlag.set || protocolFlag.set || debugFlag.set

	if cliOverrides {
		// CLI has precedence: ignore config file completely.
		if hostFlag.set && hostFlag.value != "" {
			host = hostFlag.value
		}
		if portFlag.set && portFlag.value != "" {
			port = portFlag.value
		}
		if protocolFlag.set && protocolFlag.value != "" {
			protocol = protocolFlag.value
		}
		if debugFlag.set {
			debug = debugFlag.value
		}
	} else {
		// No CLI flags: try config, then fall back to defaults.
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
			debug = cfg.Debug
		}
	}

	if debug {
		log.SetLevel(log.DebugLevel)
	}

	log.WithFields(log.Fields{
		"host":     host,
		"port":     port,
		"protocol": protocol,
		"debug":    debug,
		"config":   *configPath,
		"cli":      cliOverrides,
	}).Debug("Starting etcd-walker")

	ctrl := controller.NewController(host, port, debug, protocol)
	if err := ctrl.Run(); err != nil {
		log.WithError(err).Error("etcd-walker exited with error")
		os.Exit(1)
	}
}
