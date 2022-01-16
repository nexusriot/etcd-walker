package main

import (
	"os"

	"github.com/nexusriot/etcd-walker/pkg/controller"
	log "github.com/sirupsen/logrus"
)

func main() {
	f, err := os.OpenFile("etcd-walker.log", os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		panic("failed to open log file")
	}
	defer f.Close()

	log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(f)
	log.SetLevel(log.DebugLevel)
	controller.NewController("", "", "").Run()
}
