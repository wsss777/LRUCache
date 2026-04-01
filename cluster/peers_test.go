package cluster

import (
	"testing"

	"github.com/wsss777/LRUCache/consistentHash"
	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestPickPeerReturnsSelfWhenHashSelectsLocalNode(t *testing.T) {
	picker := &ClientPicker{
		selfAddr: "self:8001",
		consHash: consistentHash.New(),
		clients:  make(map[string]*Client),
	}
	if err := picker.consHash.Add(picker.selfAddr); err != nil {
		t.Fatalf("add self node: %v", err)
	}

	peer, ok, self := picker.PickPeer("any-key")
	if !ok {
		t.Fatal("expected picker to resolve a node")
	}
	if !self {
		t.Fatal("expected hash ring to identify the local node")
	}
	if peer != nil {
		t.Fatal("expected no remote client when local node is selected")
	}
}

func TestHandleWatchEventsRemovesPeerOnDeleteEvent(t *testing.T) {
	const peerAddr = "10.0.0.2:8001"

	picker := &ClientPicker{
		selfAddr: "10.0.0.1:8001",
		svcName:  "svc",
		consHash: consistentHash.New(),
		clients: map[string]*Client{
			peerAddr: {},
		},
	}
	if err := picker.consHash.Add(picker.selfAddr, peerAddr); err != nil {
		t.Fatalf("seed ring: %v", err)
	}

	picker.handleWatchEvents([]*clientv3.Event{
		{
			Type: clientv3.EventTypeDelete,
			Kv: &mvccpb.KeyValue{
				Key: []byte("/services/svc/" + peerAddr),
			},
		},
	})

	if _, exists := picker.clients[peerAddr]; exists {
		t.Fatal("expected deleted peer to be removed from picker")
	}
}
