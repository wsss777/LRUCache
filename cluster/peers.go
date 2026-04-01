package cluster

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/wsss777/LRUCache/consistentHash"
	"github.com/wsss777/LRUCache/logger"
	"github.com/wsss777/LRUCache/registry"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

const (
	defaultSvcName         = "ws-cache"
	defaultMigrationWindow = 10 * time.Minute
)

type PeerPicker interface {
	PickPeer(key string) (peer Peer, ok bool, self bool)
	Close() error
}

type PeerBroadcaster interface {
	AllPeers() []Peer
}

type Peer interface {
	Get(group string, key string) ([]byte, error)
	Set(ctx context.Context, group string, key string, value []byte) error
	Delete(group string, key string) (bool, error)
	Close() error
}

type MigrationPeer interface {
	Peer
	GetLocalEntry(group, key string) ([]byte, time.Time, error)
	SetWithExpireAt(ctx context.Context, group, key string, value []byte, expireAt time.Time) error
}

type MigrationAwarePicker interface {
	PeerPicker
	PickPreviousPeer(key string) (peer Peer, ok bool, self bool)
	CurrentOwner(key string) string
	PreviousOwner(key string) string
	SelfAddress() string
	RegisterTopologyChangeListener(listener func())
}

type ClientPicker struct {
	selfAddr        string
	svcName         string
	mu              sync.RWMutex
	consHash        *consistentHash.Map
	prevHash        *consistentHash.Map
	clients         map[string]*Client
	etcdCli         *clientv3.Client
	ctx             context.Context
	cancel          context.CancelFunc
	migrationWindow time.Duration
	migrationUntil  time.Time
	listeners       []func()
}

type PickerOption func(*ClientPicker)

func WithServiceName(name string) PickerOption {
	return func(p *ClientPicker) {
		p.svcName = name
	}
}

func WithMigrationWindow(window time.Duration) PickerOption {
	return func(p *ClientPicker) {
		if window > 0 {
			p.migrationWindow = window
		}
	}
}

func (p *ClientPicker) PrintPeers() {
	p.mu.RLock()
	defer p.mu.RUnlock()

	log.Printf("current discovered peers:")
	log.Printf("- self: %s", p.selfAddr)
	for addr := range p.clients {
		log.Printf("- %s", addr)
	}
}

func NewClientPicker(addr string, opts ...PickerOption) (*ClientPicker, error) {
	selfAddr, err := registry.NormalizeAddress(addr)
	if err != nil {
		return nil, fmt.Errorf("failed to normalize self address: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	picker := &ClientPicker{
		selfAddr:        selfAddr,
		svcName:         defaultSvcName,
		clients:         make(map[string]*Client),
		consHash:        consistentHash.New(),
		ctx:             ctx,
		cancel:          cancel,
		migrationWindow: defaultMigrationWindow,
	}
	for _, opt := range opts {
		opt(picker)
	}

	if err := picker.consHash.Add(selfAddr); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to add self into consistent hash: %v", err)
	}

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   registry.DefaultConfig.Endpoints,
		DialTimeout: registry.DefaultConfig.DialTimeout,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create etcd client: %v", err)
	}
	picker.etcdCli = cli

	if err := picker.startServiceDiscovery(); err != nil {
		cancel()
		cli.Close()
		return nil, fmt.Errorf("failed to start service discovery: %v", err)
	}
	return picker, nil
}

func (p *ClientPicker) RegisterTopologyChangeListener(listener func()) {
	if listener == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listeners = append(p.listeners, listener)
}

func (p *ClientPicker) SelfAddress() string {
	return p.selfAddr
}

func (p *ClientPicker) startServiceDiscovery() error {
	if err := p.fetchAllServices(); err != nil {
		return err
	}
	go p.watchServiceChanges()
	return nil
}

func (p *ClientPicker) watchServiceChanges() {
	watcher := clientv3.NewWatcher(p.etcdCli)
	defer watcher.Close()

	watchChan := watcher.Watch(p.ctx, "/services/"+p.svcName, clientv3.WithPrefix())
	for {
		select {
		case <-p.ctx.Done():
			return
		case resp, ok := <-watchChan:
			if !ok {
				return
			}
			p.handleWatchEvents(resp.Events)
		}
	}
}

func (p *ClientPicker) handleWatchEvents(events []*clientv3.Event) {
	for _, event := range events {
		addr := p.addrFromEvent(event)
		if addr == "" || addr == p.selfAddr {
			continue
		}

		switch event.Type {
		case clientv3.EventTypePut:
			if p.set(addr, true) {
				logger.L().Info("New service discovered",
					zap.String("addr", addr))
			}
		case clientv3.EventTypeDelete:
			if p.remove(addr, true) {
				logger.L().Info("Removed service discovered",
					zap.String("addr", addr))
			}
		}
	}
}

func (p *ClientPicker) fetchAllServices() error {
	ctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
	defer cancel()

	resp, err := p.etcdCli.Get(ctx, "/services/"+p.svcName, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("failed to get all services from etcd: %v", err)
	}

	for _, kv := range resp.Kvs {
		addr := string(kv.Value)
		if addr == "" || addr == p.selfAddr {
			continue
		}
		if p.set(addr, false) {
			logger.L().Info("New service discovered",
				zap.String("addr", addr))
		}
	}
	return nil
}

func (p *ClientPicker) set(addr string, notify bool) bool {
	if addr == "" || addr == p.selfAddr {
		return false
	}

	p.mu.Lock()
	if _, exists := p.clients[addr]; exists {
		p.mu.Unlock()
		return false
	}
	prevSnapshot := p.consHash.Clone()
	p.mu.Unlock()

	client, err := NewClient(addr, p.svcName, p.etcdCli)
	if err != nil {
		logger.L().Error("failed to create client",
			zap.String("addr", addr),
			zap.Error(err),
		)
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.clients[addr]; exists {
		client.Close()
		return false
	}
	if err := p.consHash.Add(addr); err != nil {
		client.Close()
		logger.L().Error("failed to add peer into consistent hash",
			zap.String("addr", addr),
			zap.Error(err),
		)
		return false
	}

	p.clients[addr] = client
	p.activatePreviousRingLocked(prevSnapshot, notify)
	logger.L().Info("successfully created client",
		zap.String("addr", addr))
	return true
}

func (p *ClientPicker) remove(addr string, notify bool) bool {
	if addr == "" || addr == p.selfAddr {
		return false
	}

	p.mu.Lock()
	client, exists := p.clients[addr]
	if !exists {
		p.mu.Unlock()
		return false
	}
	prevSnapshot := p.consHash.Clone()
	delete(p.clients, addr)
	_ = p.consHash.Remove(addr)
	p.activatePreviousRingLocked(prevSnapshot, notify)
	p.mu.Unlock()

	client.Close()
	return true
}

func (p *ClientPicker) activatePreviousRingLocked(prev *consistentHash.Map, notify bool) {
	if !notify || prev == nil {
		return
	}

	p.prevHash = prev
	p.migrationUntil = time.Now().Add(p.migrationWindow)
	listeners := append([]func(){}, p.listeners...)
	go func() {
		for _, listener := range listeners {
			listener()
		}
	}()
}

func (p *ClientPicker) currentRingLocked() *consistentHash.Map {
	return p.consHash
}

func (p *ClientPicker) previousRingLocked() *consistentHash.Map {
	if p.prevHash == nil || time.Now().After(p.migrationUntil) {
		return nil
	}
	return p.prevHash
}

func (p *ClientPicker) CurrentOwner(key string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.currentRingLocked().Get(key)
}

func (p *ClientPicker) PreviousOwner(key string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ring := p.previousRingLocked()
	if ring == nil {
		return ""
	}
	return ring.Get(key)
}

func (p *ClientPicker) PickPeer(key string) (Peer, bool, bool) {
	return p.pickFromRing(key, false)
}

func (p *ClientPicker) AllPeers() []Peer {
	p.mu.RLock()
	defer p.mu.RUnlock()

	peers := make([]Peer, 0, len(p.clients))
	for _, client := range p.clients {
		peers = append(peers, client)
	}
	return peers
}

func (p *ClientPicker) PickPreviousPeer(key string) (Peer, bool, bool) {
	return p.pickFromRing(key, true)
}

func (p *ClientPicker) pickFromRing(key string, previous bool) (Peer, bool, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var ring *consistentHash.Map
	if previous {
		ring = p.previousRingLocked()
	} else {
		ring = p.currentRingLocked()
	}
	if ring == nil {
		return nil, false, false
	}

	addr := ring.Get(key)
	if addr == "" {
		return nil, false, false
	}
	if addr == p.selfAddr {
		return nil, true, true
	}
	client, exists := p.clients[addr]
	if !exists {
		return nil, false, false
	}
	return client, true, false
}

func (p *ClientPicker) Close() error {
	p.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()

	var errs []error
	for addr, client := range p.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close client %s: %v", addr, err))
		}
	}

	if p.etcdCli != nil {
		if err := p.etcdCli.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close etcd client: %v", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors while closing: %v", errs)
	}
	return nil
}

func (p *ClientPicker) addrFromEvent(event *clientv3.Event) string {
	if event == nil || event.Kv == nil {
		return ""
	}
	if addr := string(event.Kv.Value); addr != "" {
		return addr
	}
	return parseAddrFromKey(string(event.Kv.Key), p.svcName)
}

func parseAddrFromKey(key, svcName string) string {
	prefix := fmt.Sprintf("/services/%s/", svcName)
	if strings.HasPrefix(key, prefix) {
		return strings.TrimPrefix(key, prefix)
	}
	return ""
}
