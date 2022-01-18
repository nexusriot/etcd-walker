package model

import (
	"context"
	"fmt"

	"github.com/coreos/etcd/client"
)

type Model struct {
	client client.Client
	api    client.KeysAPI
}

type Node struct {
	Name      string
	ClusterId string
	IsDir     bool
	Value     string
}

func NewModel(host string, port string) *Model {
	// TODO: make configurable
	etcd, err := client.New(client.Config{
		Endpoints: []string{fmt.Sprintf("http://%s:%s", host, port)},
	})
	api := client.NewKeysAPI(etcd)
	if err != nil {
		panic(err)
	}
	m := Model{
		client: etcd,
		api:    api,
	}
	return &m
}

func (m *Model) appendNode(nodes []*Node, name string, isDirectory bool, value string, clusterId string) ([]*Node, error) {
	node := &Node{
		Name:      name,
		IsDir:     isDirectory,
		ClusterId: clusterId,
		Value:     value,
	}
	nodes = append(nodes, node)
	return nodes, nil
}

func (m *Model) nodesToModelNodes(nodes client.Nodes, clusterId string) []*Node {
	var nds []*Node
	for _, node := range nodes {
		nds, _ = m.appendNode(nds, node.Key, node.Dir, node.Value, clusterId)
	}
	return nds
}

func (m *Model) Ls(directory string) ([]*Node, error) {
	options := &client.GetOptions{Sort: true, Recursive: false}
	response, err := m.api.Get(context.Background(), directory, options)
	if err != nil {
		if client.IsKeyNotFound(err) {
			return make([]*Node, 0), nil
		}
		return make([]*Node, 0), err
	}
	return m.nodesToModelNodes(response.Node.Nodes, response.ClusterID), nil
}
