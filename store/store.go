package store

import "time"

type Value interface {
	Len() int
}

type Store interface {
	Get(key string) (Value, bool)
	Set(key string, value Value) error
	SetWithExpiration(key string, value Value, expiration time.Duration) error
	Delete(key string) bool
	Clear()
	Len() int
	Close()
}

type CacheType string

const (
	LRU  CacheType = "lru"
	LRU2 CacheType = "lru2"
)

type Options struct {
	MaxBytes        int64  //lru
	BucketCount     uint16 //lru2
	CapPerBucket    uint16 //lru2
	Level2Cap       uint16 //lru2,二级缓存的容量
	CleanupInterval time.Duration
	OnEvicted       func(key string, value Value)
}

func NewOptions() Options {
	return Options{
		MaxBytes:        8192,
		BucketCount:     16,
		CapPerBucket:    512,
		Level2Cap:       256,
		CleanupInterval: 5 * time.Minute,
		OnEvicted:       nil,
	}
}

func NewStore(cacheType CacheType, opts Options) Store {
	switch cacheType {
	case LRU2:
		return newLRU2Cache(opts)
	case LRU:
		return newLRUCache(opts)
	default:
		return newLRUCache(opts)
	}
}
