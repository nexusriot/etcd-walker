package main

import (
	"fmt"
	"os"

	"github.com/nexusriot/etcd-walker/pkg/controller"
	log "github.com/sirupsen/logrus"
)

func main() {
	// open a file
	f, err := os.OpenFile("etcd-walker.log", os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		fmt.Printf("error opening file: %v", err)
	}

	// don't forget to close it
	defer f.Close()

	// Log as JSON instead of the default ASCII formatter.
	log.SetFormatter(&log.JSONFormatter{})

	// Output to stderr instead of stdout, could also be a file.
	log.SetOutput(f)

	// Only log the warning severity or above.
	log.SetLevel(log.DebugLevel)
	controller.NewController("", "", "").Run()

}
