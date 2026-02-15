package store

import (
	"sync"
	"time"
)

type lru2Store struct {
	locks       []sync.Mutex
	caches      [][2]*cache
	onEvicted   func(k string, v Value)
	cleanupTick *time.Ticker
	mask        int32
}

type node struct {
	key      string
	value    Value
	expireAt int64
}
type cache struct {
	dlnk [][2]uint16
	m    []node
	hmap map[string]uint16
	last uint16
}

func newLRU2Cache(opts Options) *lru2Store {
	if opts.BucketCount == 0 {
		opts.BucketCount = 16
	}
	if opts.CapPerBucket == 0 {
		opts.CapPerBucket = 1024
	}
	if opts.Level2Cap == 0 {
		opts.Level2Cap = 1024
	}
	if opts.CleanupInterval <= 0 {
		opts.CleanupInterval = time.Minute
	}

	mask := maskOfNextPowOf(opts.BucketCount)
	s := &lru2Store{
		locks:       make([]sync.Mutex, mask+1),
		caches:      make([][2]*cache, mask+1),
		onEvicted:   opts.OnEvicted,
		cleanupTick: time.NewTicker(opts.CleanupInterval),
		mask:        int32(mask),
	}
	for i := range s.caches {
		s.caches[i][0] = Create(opts.CapPerBucket)
		s.caches[i][1] = Create(opts.Level2Cap)
	}
	if opts.CleanupInterval > 0 {
		go s.cleanupLoop()
	}
	return s
}
