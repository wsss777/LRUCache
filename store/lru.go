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

func newLRUCache(opts Options) *lruCache {

}
