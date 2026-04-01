package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wsss777/LRUCache/cluster"
)

type fakeMigrationPeer struct {
	mu         sync.Mutex
	localValue []byte
	localTTL   time.Time
	setCalls   []setCall
}

type setCall struct {
	group    string
	key      string
	value    []byte
	expireAt time.Time
}

func (p *fakeMigrationPeer) Get(group, key string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.localValue...), nil
}

func (p *fakeMigrationPeer) GetLocalEntry(group, key string) ([]byte, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.localValue...), p.localTTL, nil
}

func (p *fakeMigrationPeer) Set(ctx context.Context, group, key string, value []byte) error {
	return p.SetWithExpireAt(ctx, group, key, value, time.Time{})
}

func (p *fakeMigrationPeer) SetWithExpireAt(ctx context.Context, group, key string, value []byte, expireAt time.Time) error {
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

func (p *fakeMigrationPeer) Delete(group, key string) (bool, error) {
	return true, nil
}

func (p *fakeMigrationPeer) Close() error {
	return nil
}

type fakeMigrationPicker struct {
	self      string
	current   cluster.Peer
	previous  cluster.Peer
	listeners []func()
	curOwner  string
	prevOwner string
}

func (p *fakeMigrationPicker) PickPeer(key string) (cluster.Peer, bool, bool) {
	return p.current, true, p.curOwner == p.self
}

func (p *fakeMigrationPicker) PickPreviousPeer(key string) (cluster.Peer, bool, bool) {
	return p.previous, true, p.prevOwner == p.self
}

func (p *fakeMigrationPicker) CurrentOwner(key string) string {
	return p.curOwner
}

func (p *fakeMigrationPicker) PreviousOwner(key string) string {
	return p.prevOwner
}

func (p *fakeMigrationPicker) SelfAddress() string {
	return p.self
}

func (p *fakeMigrationPicker) RegisterTopologyChangeListener(listener func()) {
	p.listeners = append(p.listeners, listener)
}

func (p *fakeMigrationPicker) emitTopologyChange() {
	for _, listener := range p.listeners {
		listener()
	}
}

func (p *fakeMigrationPicker) Close() error {
	return nil
}

func TestGroupLazyMigrationFallsBackToPreviousOwner(t *testing.T) {
	previousPeer := &fakeMigrationPeer{
		localValue: []byte("migrated-value"),
		localTTL:   time.Now().Add(5 * time.Minute),
	}
	picker := &fakeMigrationPicker{
		self:      "self",
		current:   nil,
		previous:  previousPeer,
		curOwner:  "self",
		prevOwner: "old-node",
	}

	g := NewGroup("lazy-migration", 1<<20, GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		t.Fatalf("getter should not be called for lazy migration fallback")
		return nil, nil
	}))
	t.Cleanup(func() {
		_ = g.Close()
	})
	g.RegisterPeers(picker)

	view, err := g.Get(context.Background(), "hot-key")
	if err != nil {
		t.Fatalf("get should succeed via previous owner: %v", err)
	}
	if got := view.String(); got != "migrated-value" {
		t.Fatalf("unexpected value: %s", got)
	}

	if _, expireAt, ok := g.GetLocalEntry("hot-key"); !ok || expireAt.IsZero() {
		t.Fatalf("expected migrated value to be written locally with TTL")
	}
}

func TestGroupPrewarmMigratesKeysToNewOwner(t *testing.T) {
	newOwner := &fakeMigrationPeer{}
	picker := &fakeMigrationPicker{
		self:      "self",
		current:   newOwner,
		previous:  nil,
		curOwner:  "new-node",
		prevOwner: "self",
	}

	g := NewGroup("prewarm", 1<<20, GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		return nil, nil
	}))
	t.Cleanup(func() {
		_ = g.Close()
	})
	g.RegisterPeers(picker)

	expireAt := time.Now().Add(2 * time.Minute).Round(0)
	if err := g.SetWithExpireAt(cluster.WithPeerRequest(context.Background()), "prewarm-key", []byte("value"), expireAt); err != nil {
		t.Fatalf("seed local cache: %v", err)
	}

	picker.emitTopologyChange()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		newOwner.mu.Lock()
		calls := append([]setCall(nil), newOwner.setCalls...)
		newOwner.mu.Unlock()
		if len(calls) > 0 {
			if calls[0].key != "prewarm-key" {
				t.Fatalf("unexpected migrated key: %s", calls[0].key)
			}
			if calls[0].expireAt.IsZero() {
				t.Fatal("expected prewarm migration to preserve expiration")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("expected prewarm migration to replicate the key")
}
