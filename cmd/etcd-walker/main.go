package main

import (
	"flag"
	"os"
	"strconv"

	"github.com/nexusriot/etcd-walker/pkg/controller"
	log "github.com/sirupsen/logrus"
)

func main() {

	hostname := flag.String("host", "localhost", "host name")
	port := flag.Int("port", 2379, "port number")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()

	f, err := os.OpenFile("etcd-walker.log", os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		panic("failed to open log file")
	}
	defer f.Close()

	log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(f)

	if *debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	controller.NewController(*hostname, strconv.Itoa(*port), *debug).Run()
}
