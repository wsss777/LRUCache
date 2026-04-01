package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wsss777/LRUCache/cluster"
)

type syncSpyPeer struct {
	mu          sync.Mutex
	setCalls    []setCall
	deleteCalls []string
	getValue    []byte
}

func (p *syncSpyPeer) Get(group, key string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.getValue...), nil
}

func (p *syncSpyPeer) Set(ctx context.Context, group, key string, value []byte) error {
	return p.SetWithExpireAt(ctx, group, key, value, time.Time{})
}

func (p *syncSpyPeer) SetWithExpireAt(ctx context.Context, group, key string, value []byte, expireAt time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setCalls = append(p.setCalls, setCall{
		group:    group,
		key:      key,
		value:    append([]byte(nil), value...),
		expireAt: expireAt,
	})
	return nil
}

func (p *syncSpyPeer) GetLocalEntry(group, key string) ([]byte, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.getValue...), time.Time{}, nil
}

func (p *syncSpyPeer) Delete(group, key string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleteCalls = append(p.deleteCalls, key)
	return true, nil
}

func (p *syncSpyPeer) Close() error {
	return nil
}

type broadcastPicker struct {
	owner cluster.Peer
	peers []cluster.Peer
}

func (p *broadcastPicker) PickPeer(key string) (cluster.Peer, bool, bool) {
	return p.owner, true, false
}

func (p *broadcastPicker) AllPeers() []cluster.Peer {
	return append([]cluster.Peer(nil), p.peers...)
}

func (p *broadcastPicker) Close() error {
	return nil
}

func TestGroupSetBroadcastsToPeersWithCachedCopies(t *testing.T) {
	owner := &syncSpyPeer{getValue: []byte("old")}
	reader := &syncSpyPeer{}
	picker := &broadcastPicker{
		owner: owner,
		peers: []cluster.Peer{owner, reader},
	}

	g := NewGroup("sync-set-all-peers", 1<<20, GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		t.Fatalf("getter should not be called")
		return nil, nil
	}), WithPeers(picker))
	t.Cleanup(func() {
		_ = g.Close()
	})

	if _, err := g.Get(context.Background(), "k"); err != nil {
		t.Fatalf("seed local cache from owner: %v", err)
	}

	if err := g.Set(context.Background(), "k", []byte("new")); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		owner.mu.Lock()
		ownerCalls := append([]setCall(nil), owner.setCalls...)
		owner.mu.Unlock()

		reader.mu.Lock()
		readerCalls := append([]setCall(nil), reader.setCalls...)
		reader.mu.Unlock()

		if len(ownerCalls) > 0 && len(readerCalls) > 0 {
			if string(ownerCalls[0].value) != "new" {
				t.Fatalf("unexpected owner value: %s", string(ownerCalls[0].value))
			}
			if string(readerCalls[0].value) != "new" {
				t.Fatalf("unexpected reader value: %s", string(readerCalls[0].value))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("expected update to be broadcast to owner and reader peer")
}

func TestGroupDeleteBroadcastsToPeersWithCachedCopies(t *testing.T) {
	owner := &syncSpyPeer{}
	reader := &syncSpyPeer{}
	picker := &broadcastPicker{
		owner: owner,
		peers: []cluster.Peer{owner, reader},
	}

	g := NewGroup("sync-delete-all-peers", 1<<20, GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		return []byte("value"), nil
	}), WithPeers(picker))
	t.Cleanup(func() {
		_ = g.Close()
	})

	if err := g.Delete(context.Background(), "k"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		owner.mu.Lock()
		ownerCalls := append([]string(nil), owner.deleteCalls...)
		owner.mu.Unlock()

		reader.mu.Lock()
		readerCalls := append([]string(nil), reader.deleteCalls...)
		reader.mu.Unlock()

		if len(ownerCalls) > 0 && len(readerCalls) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("expected delete to be broadcast to owner and reader peer")
}
