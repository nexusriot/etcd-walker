package model

import (
	"context"
	"github.com/coreos/etcd/client"
)

type Model struct {
	client client.Client
	api    client.KeysAPI
}

func NewModel() *Model {
	etcd, err := client.New(client.Config{
		Endpoints: []string{"http://127.0.0.1:2379"},
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

func (m *Model) ListRoot() ([]string, error) {
	options := &client.GetOptions{Sort: true, Recursive: true}
	//_, err = api.Set(context.Background(), "ololo", "", &client.SetOptions{Dir: true, PrevExist: client.PrevIgnore})
	response, err := m.api.Get(context.Background(), "/", options)
	if err != nil {
		return nil, err
	}
	var keys []string
	if response.Node.Nodes != nil {
		for _, v := range response.Node.Nodes {
			keys = append(keys, v.Key)
		}
	}
	return keys, nil
}

func Misc() {
	//options := &client.GetOptions{Sort: true, Recursive: true}
	//_, err = api.Set(context.Background(), "ololo", "", &client.SetOptions{Dir: true, PrevExist: client.PrevIgnore})
	//response, err := api.Get(context.Background(), "/", options)
	//_, err = api.Set(context.Background(), "/ololo/pp", "olol\nlo\nlo", nil)
	//
	//response, err = api.Get(context.Background(), "/ololo/pp", nil)
}
