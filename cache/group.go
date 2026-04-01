package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wsss777/LRUCache/cluster"
	"github.com/wsss777/LRUCache/logger"
	"github.com/wsss777/LRUCache/singleFlight"
	"go.uber.org/zap"
)

var (
	groupsMu sync.RWMutex
	groups   = make(map[string]*Group)
)

var ErrKeyRequired = errors.New("key is required")
var ErrValueRequired = errors.New("value is required")
var ErrGroupClosed = errors.New("cache group is closed")

type Getter interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

type GetterFunc func(ctx context.Context, key string) ([]byte, error)

func (f GetterFunc) Get(ctx context.Context, key string) ([]byte, error) {
	return f(ctx, key)
}

type Group struct {
	name             string
	getter           Getter
	mainCache        *Cache
	peers            cluster.PeerPicker
	loader           *singleFlight.Group
	expiration       time.Duration
	closed           int32
	stats            groupStats
	migrationRunning int32
}

type groupStats struct {
	loads             int64
	localHits         int64
	localMisses       int64
	peerHits          int64
	peerMisses        int64
	loaderHits        int64
	loaderErrors      int64
	loadDuration      int64
	migrationFallback int64
	migrationPrewarm  int64
}

type GroupOption func(*Group)

func WithExpiration(d time.Duration) GroupOption {
	return func(g *Group) {
		g.expiration = d
	}
}

func WithPeers(peers cluster.PeerPicker) GroupOption {
	return func(g *Group) {
		g.peers = peers
	}
}

func WithCacheOptions(opts CacheOptions) GroupOption {
	return func(g *Group) {
		g.mainCache = NewCache(opts)
	}
}

func NewGroup(name string, cacheBytes int64, getter Getter, opts ...GroupOption) *Group {
	if getter == nil {
		panic("nil getter")
	}

	cacheOpts := DefaultCacheOptions()
	cacheOpts.MaxBytes = cacheBytes

	g := &Group{
		name:      name,
		getter:    getter,
		mainCache: NewCache(cacheOpts),
		loader:    &singleFlight.Group{},
	}
	for _, opt := range opts {
		opt(g)
	}

	groupsMu.Lock()
	defer groupsMu.Unlock()

	if _, exists := groups[name]; exists {
		logger.L().Warn("Group with name already exists , will be replaced",
			zap.String("name", name))
	}
	groups[name] = g
	logger.L().Info("Created cache group",
		zap.String("name", name),
		zap.Int64("cacheBytes", cacheBytes),
		zap.Any("expiration", g.expiration),
	)
	return g
}

func GetGroup(name string) *Group {
	groupsMu.RLock()
	defer groupsMu.RUnlock()
	return groups[name]
}

func (g *Group) Get(ctx context.Context, key string) (ByteView, error) {
	if atomic.LoadInt32(&g.closed) == 1 {
		return ByteView{}, ErrGroupClosed
	}
	if key == "" {
		return ByteView{}, ErrKeyRequired
	}

	view, ok := g.mainCache.Get(ctx, key)
	if ok {
		atomic.AddInt64(&g.stats.localHits, 1)
		return view, nil
	}
	atomic.AddInt64(&g.stats.localMisses, 1)
	return g.load(ctx, key)
}

func (g *Group) GetLocalEntry(key string) (ByteView, time.Time, bool) {
	if atomic.LoadInt32(&g.closed) == 1 {
		return ByteView{}, time.Time{}, false
	}
	return g.mainCache.GetEntry(key)
}

func (g *Group) Set(ctx context.Context, key string, value []byte) error {
	return g.setInternal(ctx, key, value, time.Time{})
}

func (g *Group) SetWithExpireAt(ctx context.Context, key string, value []byte, expireAt time.Time) error {
	return g.setInternal(ctx, key, value, expireAt)
}

func (g *Group) setInternal(ctx context.Context, key string, value []byte, expireAt time.Time) error {
	if atomic.LoadInt32(&g.closed) == 1 {
		return ErrGroupClosed
	}
	if key == "" {
		return ErrKeyRequired
	}
	if len(value) == 0 {
		return ErrValueRequired
	}

	isPeerRequest := cluster.IsPeerRequest(ctx)
	view := ByteView{b: cloneBytes(value)}
	g.storeLocally(key, view, expireAt)

	if !isPeerRequest && g.peers != nil {
		go g.syncToPeers(ctx, "set", key, value, expireAt)
	}
	return nil
}

func (g *Group) storeLocally(key string, value ByteView, expireAt time.Time) {
	if !expireAt.IsZero() {
		g.mainCache.AddWithExpireAt(key, value, expireAt)
		return
	}
	if g.expiration > 0 {
		g.mainCache.AddWithExpiration(key, value, time.Now().Add(g.expiration))
		return
	}
	g.mainCache.Add(key, value)
}

func (g *Group) Delete(ctx context.Context, key string) error {
	if atomic.LoadInt32(&g.closed) == 1 {
		return ErrGroupClosed
	}
	if key == "" {
		return ErrKeyRequired
	}

	g.mainCache.Delete(key)
	isPeerRequest := cluster.IsPeerRequest(ctx)
	if !isPeerRequest && g.peers != nil {
		go g.syncToPeers(ctx, "delete", key, nil, time.Time{})
	}
	return nil
}

func (g *Group) syncToPeers(ctx context.Context, op string, key string, value []byte, expireAt time.Time) {
	if g.peers == nil {
		return
	}

	var peers []cluster.Peer
	if broadcaster, ok := g.peers.(cluster.PeerBroadcaster); ok {
		peers = broadcaster.AllPeers()
	} else {
		peer, ok, isSelf := g.peers.PickPeer(key)
		if !ok || isSelf {
			return
		}
		peers = append(peers, peer)
	}

	if len(peers) == 0 {
		return
	}

	syncCtx := cluster.WithPeerRequest(context.Background())
	for _, peer := range peers {
		if peer == nil {
			continue
		}

		var err error
		switch op {
		case "set":
			if migrationPeer, ok := peer.(cluster.MigrationPeer); ok {
				err = migrationPeer.SetWithExpireAt(syncCtx, g.name, key, value, expireAt)
			} else {
				err = peer.Set(syncCtx, g.name, key, value)
			}
		case "delete":
			_, err = peer.Delete(g.name, key)
		}
		if err != nil {
			logger.L().Error("Error in syncToPeers",
				zap.String("op", op),
				zap.String("key", key),
				zap.Error(err))
		}
	}
}

func (g *Group) Clear() {
	if atomic.LoadInt32(&g.closed) == 1 {
		return
	}
	g.mainCache.Clear()
	logger.L().Info("Group Clear cache",
		zap.String("name", g.name))
}

func (g *Group) Close() error {
	if !atomic.CompareAndSwapInt32(&g.closed, 0, 1) {
		return nil
	}
	if g.mainCache != nil {
		g.mainCache.Close()
	}

	groupsMu.Lock()
	delete(groups, g.name)
	groupsMu.Unlock()
	logger.L().Info("Group closed",
		zap.String("name", g.name))
	return nil
}

func (g *Group) load(ctx context.Context, key string) (value ByteView, err error) {
	startTime := time.Now()
	viewi, err := g.loader.Do(key, func() (interface{}, error) {
		return g.loadData(ctx, key)
	})
	loadDuration := time.Since(startTime).Nanoseconds()
	atomic.AddInt64(&g.stats.loadDuration, loadDuration)
	atomic.AddInt64(&g.stats.loads, 1)

	if err != nil {
		atomic.AddInt64(&g.stats.loaderErrors, 1)
		return ByteView{}, err
	}
	return viewi.(ByteView), nil
}

func (g *Group) loadData(ctx context.Context, key string) (value ByteView, err error) {
	if g.peers != nil {
		peer, ok, isSelf := g.peers.PickPeer(key)
		if ok && !isSelf {
			value, err := g.getFromPeer(ctx, peer, key)
			if err == nil {
				atomic.AddInt64(&g.stats.peerHits, 1)
				return value, nil
			}

			atomic.AddInt64(&g.stats.peerMisses, 1)
			logger.L().Error("failed to get data",
				zap.String("key", key),
				zap.Error(err))

			if migrated, migratedErr := g.tryLazyMigrateFromPreviousPeer(ctx, key); migratedErr == nil {
				return migrated, nil
			}
		} else if ok && isSelf {
			if migrated, migratedErr := g.tryLazyMigrateFromPreviousPeer(ctx, key); migratedErr == nil {
				return migrated, nil
			}
		}
	}

	bytes, err := g.getter.Get(ctx, key)
	if err != nil {
		return ByteView{}, fmt.Errorf("failed to get from peer : %w", err)
	}
	atomic.AddInt64(&g.stats.loaderHits, 1)
	view := ByteView{b: cloneBytes(bytes)}
	g.storeLocally(key, view, time.Time{})
	return view, nil
}

func (g *Group) tryLazyMigrateFromPreviousPeer(ctx context.Context, key string) (ByteView, error) {
	migrationPicker, ok := g.peers.(cluster.MigrationAwarePicker)
	if !ok {
		return ByteView{}, errors.New("migration-aware picker unavailable")
	}

	oldPeer, ok, isSelf := migrationPicker.PickPreviousPeer(key)
	if !ok || isSelf {
		return ByteView{}, errors.New("previous owner unavailable")
	}

	migrationPeer, ok := oldPeer.(cluster.MigrationPeer)
	if !ok {
		return ByteView{}, errors.New("previous peer does not support entry fetch")
	}

	bytes, expireAt, err := migrationPeer.GetLocalEntry(g.name, key)
	if err != nil {
		return ByteView{}, err
	}
	if len(bytes) == 0 {
		return ByteView{}, errors.New("previous owner has no local value")
	}

	view := ByteView{b: cloneBytes(bytes)}
	g.storeLocally(key, view, expireAt)
	atomic.AddInt64(&g.stats.migrationFallback, 1)
	return view, nil
}

func (g *Group) getFromPeer(ctx context.Context, peer cluster.Peer, key string) (ByteView, error) {
	bytes, err := peer.Get(g.name, key)
	if err != nil {
		return ByteView{}, fmt.Errorf("failed to get from peer : %w", err)
	}
	view := ByteView{b: cloneBytes(bytes)}
	g.storeLocally(key, view, time.Time{})
	return view, nil
}

func (g *Group) RegisterPeers(peers cluster.PeerPicker) {
	if g.peers != nil {
		panic("RegisterPeers called more than once")
	}
	g.peers = peers
	if aware, ok := peers.(cluster.MigrationAwarePicker); ok {
		aware.RegisterTopologyChangeListener(func() {
			g.startPrewarmMigration()
		})
	}
	logger.L().Info("Group RegisterPeers",
		zap.String("peer", g.name))
}

func (g *Group) startPrewarmMigration() {
	if !atomic.CompareAndSwapInt32(&g.migrationRunning, 0, 1) {
		return
	}

	go func() {
		defer atomic.StoreInt32(&g.migrationRunning, 0)

		aware, ok := g.peers.(cluster.MigrationAwarePicker)
		if !ok {
			return
		}
		selfAddr := aware.SelfAddress()
		type entry struct {
			key      string
			value    ByteView
			expireAt time.Time
		}
		batch := make([]entry, 0, 128)
		g.mainCache.Walk(func(key string, value ByteView, expireAt time.Time) bool {
			if aware.PreviousOwner(key) != selfAddr {
				return true
			}
			if aware.CurrentOwner(key) == selfAddr {
				return true
			}
			batch = append(batch, entry{
				key:      key,
				value:    value,
				expireAt: expireAt,
			})
			return true
		})

		for _, item := range batch {
			peer, ok, isSelf := g.peers.PickPeer(item.key)
			if !ok || isSelf {
				continue
			}
			migrationPeer, ok := peer.(cluster.MigrationPeer)
			if !ok {
				continue
			}
			ctx, cancel := context.WithTimeout(cluster.WithPeerRequest(context.Background()), 3*time.Second)
			err := migrationPeer.SetWithExpireAt(ctx, g.name, item.key, item.value.ByteSlice(), item.expireAt)
			cancel()
			if err != nil {
				logger.L().Warn("prewarm migration failed",
					zap.String("group", g.name),
					zap.String("key", item.key),
					zap.Error(err))
				continue
			}
			atomic.AddInt64(&g.stats.migrationPrewarm, 1)
		}
	}()
}

func (g *Group) Stats() map[string]interface{} {
	stats := map[string]interface{}{
		"name":               g.name,
		"closed":             atomic.LoadInt32(&g.closed) == 1,
		"expiration":         g.expiration,
		"loads":              atomic.LoadInt64(&g.stats.loads),
		"local_hits":         atomic.LoadInt64(&g.stats.localHits),
		"local_misses":       atomic.LoadInt64(&g.stats.localMisses),
		"peer_hits":          atomic.LoadInt64(&g.stats.peerHits),
		"peer_misses":        atomic.LoadInt64(&g.stats.peerMisses),
		"loader_hits":        atomic.LoadInt64(&g.stats.loaderHits),
		"loader_errors":      atomic.LoadInt64(&g.stats.loaderErrors),
		"migration_fallback": atomic.LoadInt64(&g.stats.migrationFallback),
		"migration_prewarm":  atomic.LoadInt64(&g.stats.migrationPrewarm),
	}

	totalGets := stats["local_hits"].(int64) + stats["local_misses"].(int64)
	if totalGets > 0 {
		stats["hit_rate"] = float64(stats["local_hits"].(int64)) / float64(totalGets)
	}

	totalLoads := stats["loads"].(int64)
	if totalLoads > 0 {
		stats["avg_load_time_ms"] = float64(atomic.LoadInt64(&g.stats.loadDuration)) / float64(totalLoads) / float64(time.Millisecond)
	}

	if g.mainCache != nil {
		cacheStats := g.mainCache.Stats()
		for k, v := range cacheStats {
			stats["cache_"+k] = v
		}
	}

	return stats
}

func ListGroups() []string {
	groupsMu.RLock()
	defer groupsMu.RUnlock()

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}

	return names
}

func DestroyGroup(name string) bool {
	groupsMu.Lock()
	defer groupsMu.Unlock()

	if g, exists := groups[name]; exists {
		g.Close()
		delete(groups, name)
		logger.L().Info("Group destroyed", zap.String("group", name))
		return true
	}
	return false
}

func DestroyAllGroups() {
	groupsMu.Lock()
	defer groupsMu.Unlock()

	for name, g := range groups {
		g.Close()
		delete(groups, name)
		logger.L().Info("Group destroyed", zap.String("group", name))
	}
}
