package model

import (
	"context"
	"fmt"
	"strings"

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

func (m *Model) RenameDir(oldDir, newDir string) error {
	ctx := context.Background()

	// Normalize (no trailing slash in keys in this app)
	if oldDir == newDir {
		return nil
	}

	// Ensure source exists
	srcResp, err := m.api.Get(ctx, oldDir, &client.GetOptions{Recursive: true})
	if err != nil {
		return err
	}
	if !srcResp.Node.Dir {
		return fmt.Errorf("source is not a directory: %s", oldDir)
	}

	// Ensure target does not exist
	if _, err := m.api.Get(ctx, newDir, nil); err == nil {
		return fmt.Errorf("target already exists: %s", newDir)
	} else if !client.IsKeyNotFound(err) {
		return err
	}

	// Create target root dir
	if _, err := m.api.Set(ctx, newDir, "", &client.SetOptions{Dir: true, PrevExist: client.PrevIgnore}); err != nil {
		return err
	}

	// Recursively copy tree to new path
	var copyTree func(n *client.Node) error
	copyTree = func(n *client.Node) error {
		// Skip the root itself; handle children
		if n.Key == oldDir {
			for _, ch := range n.Nodes {
				if err := copyTree(ch); err != nil {
					return err
				}
			}
			return nil
		}

		targetKey := strings.Replace(n.Key, oldDir, newDir, 1)
		if n.Dir {
			if _, err := m.api.Set(ctx, targetKey, "", &client.SetOptions{Dir: true, PrevExist: client.PrevIgnore}); err != nil && !client.IsKeyNotFound(err) {
				return err
			}
			for _, ch := range n.Nodes {
				if err := copyTree(ch); err != nil {
					return err
				}
			}
			return nil
		}

		// leaf value
		_, err := m.api.Set(ctx, targetKey, n.Value, nil)
		return err
	}

	if err := copyTree(srcResp.Node); err != nil {
		return err
	}

	// Delete old directory recursively
	_, err = m.api.Delete(ctx, oldDir, &client.DeleteOptions{Dir: true, Recursive: true})
	return err
}
