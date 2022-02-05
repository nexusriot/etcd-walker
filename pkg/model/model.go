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

func (m *Model) Set(key, value string) error {
	_, err := m.api.Set(context.Background(), key, value, nil)
	return err
}

func (m *Model) MkDir(directory string) error {
	res, err := m.api.Get(context.Background(), directory, nil)

	if err != nil && !client.IsKeyNotFound(err) {
		return err
	}

	if err != nil && client.IsKeyNotFound(err) {
		_, err = m.api.Set(context.Background(), directory, "", &client.SetOptions{Dir: true, PrevExist: client.PrevIgnore})
		return err
	}

	if !res.Node.Dir {
		return fmt.Errorf("trying to rewrite existing value with dir: %v", directory)
	}

	return nil
}

func (m *Model) Del(key string) error {
	_, err := m.api.Delete(context.Background(), key, nil)
	if err != nil {
		if client.IsKeyNotFound(err) {
			return nil
		}
	}
	return err
}

func (m *Model) DelDir(key string) error {
	_, err := m.api.Delete(context.Background(), key, &client.DeleteOptions{Dir: true, Recursive: true})
	if err != nil {
		if client.IsKeyNotFound(err) {
			return nil
		}
	}
	return err
}
