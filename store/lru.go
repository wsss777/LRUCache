package store

import (
	"container/list"
	"sync"
	"time"
)

// 基于 list 的 LRU 缓存
type lruCache struct {
	mu              sync.RWMutex
	list            *list.List               //双向链表
	items           map[string]*list.Element //键到链表节点的映射
	expires         map[string]time.Time     //过期时间映射
	maxBytes        int64                    //最大允许字节数
	usedBytes       int64                    //当前使用的字节数
	onEvicted       func(key string, value Value)
	cleanupInterval time.Duration
	cleanupTicker   *time.Ticker
	closeCh         chan struct{}
}

type lruEntry struct {
	key   string
	value Value
}

// 创建新的LRU缓存实例
func newLRUCache(opts Options) *lruCache {
	cleanupInterval := opts.CleanupInterval
	if cleanupInterval <= 0 {
		cleanupInterval = time.Minute
	}

	c := &lruCache{
		list:            list.New(),
		items:           make(map[string]*list.Element),
		expires:         make(map[string]time.Time),
		maxBytes:        opts.MaxBytes,
		onEvicted:       opts.OnEvicted,
		cleanupInterval: cleanupInterval,
		closeCh:         make(chan struct{}),
	}

	c.cleanupTicker = time.NewTicker(cleanupInterval)
	go c.cleanupLoop()

	return c
}

// Get 获取键值对
func (c *lruCache) Get(key string) (Value, bool) {
	c.mu.RLock()
	elem, ok := c.items[key]
	if !ok {
		c.mu.RUnlock()
		return nil, false
	}
	if expTime, hasExp := c.expires[key]; hasExp && time.Now().After(expTime) {
		c.mu.RUnlock()
		go c.Delete(key)
		return nil, false
	}

	entry := elem.Value.(*lruEntry)
	value := entry.value
	c.mu.RUnlock()
	c.mu.Lock()
	if _, ok := c.items[key]; ok {
		c.list.MoveToFront(elem)
	}
	c.mu.Unlock()
	return value, true

}

// Set  添加或更新缓存项
func (c *lruCache) Set(key string, value Value) error {
	return c.SetWithExpiration(key, value, 0)
}

// SetWithExpiration 添加或更新缓存项，并设置过期时间
func (c *lruCache) SetWithExpiration(key string, value Value, expiration time.Duration) error {
	if value == nil {
		c.Delete(key)
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	var expTime time.Time
	if expiration > 0 {
		expTime = time.Now().Add(expiration)
		c.expires[key] = expTime
	} else {
		delete(c.expires, key)
	}

	if elem, ok := c.items[key]; ok {
		oldEntry := elem.Value.(*lruEntry)
		c.usedBytes += int64(value.Len() - oldEntry.value.Len())
		oldEntry.value = value
		c.list.MoveToFront(elem)
		return nil
	}

	entry := &lruEntry{key, value}
	elem := c.list.PushFront(entry)
	c.items[key] = elem
	c.usedBytes += int64(len(key) + value.Len())
	c.evict()
	return nil
}

// Delete 从缓存中删除指定的键值
func (c *lruCache) Delete(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		c.removeElement(elem)
		return true
	}
	return false
}

// Clear 清空缓存
func (c *lruCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 如果设置了回调函数，遍历所有项调用回调
	if c.onEvicted != nil {
		for _, elem := range c.items {
			entry := elem.Value.(*lruEntry)
			c.onEvicted(entry.key, entry.value)
		}
	}
	c.list.Init()
	c.items = make(map[string]*list.Element)
	c.expires = make(map[string]time.Time)
	c.usedBytes = 0
}

// Len 返回缓存中的项数
func (c *lruCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.list.Len()
}

// removeElement 从缓存中删除元素
func (c *lruCache) removeElement(elem *list.Element) {
	entry := elem.Value.(*lruEntry)
	c.list.Remove(elem)
	delete(c.items, entry.key)
	delete(c.expires, entry.key)
	c.usedBytes -= int64(len(entry.key) + entry.value.Len())

	if c.onEvicted != nil {
		c.onEvicted(entry.key, entry.value)
	}
}

// evict 清理过期和超出内存限制的缓存，调用此方法前必须持有锁
func (c *lruCache) evict() {
	now := time.Now()
	for key, expTime := range c.expires {
		if now.After(expTime) {
			if elem, ok := c.items[key]; ok {
				c.removeElement(elem)
			}
		}
	}

	for c.maxBytes > 0 && c.usedBytes > c.maxBytes && c.list.Len() > 0 {
		elem := c.list.Back()
		if elem != nil {
			c.removeElement(c.list.Front())
		}
	}
}

// cleanupLoop定期清理过期缓存的协程
func (c *lruCache) cleanupLoop() {
	for {
		select {
		case <-c.cleanupTicker.C:
			c.mu.Lock()
			c.evict()
			c.mu.Unlock()
		case <-c.closeCh:
			return
		}

	}
}

// Close 关闭缓存，停止清理协程
func (c *lruCache) Close() {
	if c.cleanupTicker != nil {
		c.cleanupTicker.Stop()
		close(c.closeCh)
	}
}

// GetWithExpiration 获取缓存项及其剩余过期时间
func (c *lruCache) GetWithExpiration(key string) (Value, time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[key]
	if !ok {
		return nil, 0, false
	}

	now := time.Now()
	if expTime, hasExp := c.expires[key]; hasExp {
		if now.After(expTime) {
			return nil, 0, false
		}
		ttl := expTime.Sub(now)
		c.list.MoveToFront(elem)
		return elem.Value.(*lruEntry).value, ttl, true
	}
	c.list.MoveToFront(elem)
	return elem.Value.(*lruEntry).value, 0, true
}

// GetExpiration 获取键的过期时间
func (c *lruCache) GetExpiration(key string) (time.Time, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	expTime, ok := c.expires[key]
	return expTime, ok
}

// UpdateExpiration 更新过期时间
func (c *lruCache) UpdateExpiration(key string, expiration time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; !ok {
		return false
	}
	if expiration > 0 {
		c.expires[key] = time.Now().Add(expiration)
	} else {
		delete(c.expires, key)
	}
	return true
}

// UsedBytes 返回当前使用的字节数
func (c *lruCache) UsedBytes() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.usedBytes
}

// MaxBytes 返回最大允许字节数
func (c *lruCache) MaxBytes() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.maxBytes
}

// SetMaxBytes 设置最大允许字节数并触发淘汰
func (c *lruCache) SetMaxBytes(maxBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxBytes = maxBytes
	if maxBytes > 0 {
		c.evict()
	}
}
