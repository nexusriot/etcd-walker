package main

import "github.com/nexusriot/etcd-walker/pkg/controller"

func main() {

	controller.NewController("", "", "").Run()

}
