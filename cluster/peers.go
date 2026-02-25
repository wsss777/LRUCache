package cluster

import (
	"context"
	"sync"

	"github.com/wsss777/LRUCache/consistentHash"
)

const defaultSvcName = "ws-LRU-cache"

// PeerPicker 定义了peer选择器的接口
type PeerPicker interface {
	PickPeer(key string) (peer Peer, ok bool, self bool)
	Close() error
}

// Peer 定义了缓存节点的接口
type Peer interface {
	Get(group string, key string) ([]byte, error)
	Set(ctx context.Context, group string, key string, value []byte) error
	Delete(group string, key string) (bool, error)
	Close() error
}

// ClientPicker 实现了PeerPicker接口
type ClientPicker struct {
	selfAddr string
	svcName  string
	mu       sync.RWMutex
	consHash *consistentHash.Map
	clients  map[string]*Client
}
