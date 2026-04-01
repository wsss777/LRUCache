package store

import (
	"sync"
	"time"
)

const (
	Head = 0 // 链表头部，表示最近使用的节点
	Tail = 1 // 链表尾部，表示最久未使用的节点
)

type lru2Store struct {
	locks       []sync.Mutex
	caches      [][2]*cache
	onEvicted   func(k string, v Value)
	cleanupTick *time.Ticker
	closeCh     chan struct{}
	mask        int32
}

type node struct {
	k        string
	v        Value
	expireAt int64 // 过期时间戳，0 表示节点已删除
}

type cache struct {
	dlnk [][2]uint16       // 双向链表，[0] 为前驱，[1] 为后继
	m    []node            // 预分配的节点数组
	hmap map[string]uint16 // 键到节点索引的映射
	last uint16            // 当前已经分配的最大索引
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

	mask := maskOfNextPowOf2(opts.BucketCount)
	s := &lru2Store{
		locks:       make([]sync.Mutex, mask+1),
		caches:      make([][2]*cache, mask+1),
		onEvicted:   opts.OnEvicted,
		cleanupTick: time.NewTicker(opts.CleanupInterval),
		closeCh:     make(chan struct{}),
		mask:        int32(mask),
	}
	for i := range s.caches {
		s.caches[i][0] = Create(opts.CapPerBucket)
		s.caches[i][1] = Create(opts.Level2Cap)
	}

	go s.cleanupLoop()
	return s
}

// Get 先查热点层，未命中再查一级缓存；一级命中后会提升到热点层。
func (s *lru2Store) Get(key string) (Value, bool) {
	idx := hashBKRD(key) & s.mask
	s.locks[idx].Lock()
	defer s.locks[idx].Unlock()

	if hot, ok := s._get(key, idx, 1); ok > 0 && hot != nil {
		return hot.v, true
	}

	candidate, status, expireAt := s.caches[idx][0].del(key)
	if status == 0 || candidate == nil {
		return nil, false
	}
	if expireAt <= 0 || Now() >= expireAt {
		return nil, false
	}

	s.caches[idx][1].put(key, candidate.v, expireAt, s.onEvicted)
	return candidate.v, true
}

func (s *lru2Store) GetEntry(key string) (Value, time.Time, bool) {
	idx := hashBKRD(key) & s.mask
	s.locks[idx].Lock()
	defer s.locks[idx].Unlock()

	if hot, ok := s._get(key, idx, 1); ok > 0 && hot != nil {
		return hot.v, expireAtToTime(hot.expireAt), true
	}

	if cold, ok := s._get(key, idx, 0); ok > 0 && cold != nil {
		return cold.v, expireAtToTime(cold.expireAt), true
	}

	return nil, time.Time{}, false
}

func (s *lru2Store) Set(key string, value Value) error {
	return s.SetWithExpiration(key, value, 365*24*time.Hour)
}

// SetWithExpiration 新写入的数据先进入一级缓存；如果数据已经是热点，则直接更新热点层。
func (s *lru2Store) SetWithExpiration(key string, value Value, expiration time.Duration) error {
	if value == nil {
		s.Delete(key)
		return nil
	}

	expireAt := int64(0)
	if expiration > 0 {
		expireAt = Now() + int64(expiration)
	}

	idx := hashBKRD(key) & s.mask
	s.locks[idx].Lock()
	defer s.locks[idx].Unlock()

	if _, ok := s._get(key, idx, 1); ok > 0 {
		s.caches[idx][1].put(key, value, expireAt, s.onEvicted)
		return nil
	}

	s.caches[idx][0].put(key, value, expireAt, s.onEvicted)
	return nil
}

func (s *lru2Store) Delete(key string) bool {
	idx := hashBKRD(key) & s.mask
	s.locks[idx].Lock()
	defer s.locks[idx].Unlock()

	return s.delete(key, idx)
}

func (s *lru2Store) Walk(fn WalkFunc) {
	currentTime := Now()
	seen := make(map[string]struct{})
	stopped := false

	for i := range s.caches {
		if stopped {
			return
		}
		s.locks[i].Lock()
		s.caches[i][1].walk(func(key string, value Value, expireAt int64) bool {
			if expireAt > 0 && currentTime >= expireAt {
				return true
			}
			seen[key] = struct{}{}
			if !fn(key, value, expireAtToTime(expireAt)) {
				stopped = true
				return false
			}
			return true
		})
		s.caches[i][0].walk(func(key string, value Value, expireAt int64) bool {
			if _, ok := seen[key]; ok {
				return true
			}
			if expireAt > 0 && currentTime >= expireAt {
				return true
			}
			seen[key] = struct{}{}
			if !fn(key, value, expireAtToTime(expireAt)) {
				stopped = true
				return false
			}
			return true
		})
		s.locks[i].Unlock()
	}
}

func (s *lru2Store) Clear() {
	var keys []string
	for i := range s.caches {
		s.locks[i].Lock()

		s.caches[i][0].walk(func(key string, value Value, expireAt int64) bool {
			keys = append(keys, key)
			return true
		})
		s.caches[i][1].walk(func(key string, value Value, expireAt int64) bool {
			for _, existed := range keys {
				if existed == key {
					return true
				}
			}
			keys = append(keys, key)
			return true
		})

		s.locks[i].Unlock()
	}

	for _, key := range keys {
		s.Delete(key)
	}
}

func (s *lru2Store) Len() int {
	count := 0
	for i := range s.caches {
		s.locks[i].Lock()

		s.caches[i][0].walk(func(key string, value Value, expireAt int64) bool {
			count++
			return true
		})
		s.caches[i][1].walk(func(key string, value Value, expireAt int64) bool {
			count++
			return true
		})

		s.locks[i].Unlock()
	}
	return count
}

func (s *lru2Store) Close() {
	if s.cleanupTick != nil {
		s.cleanupTick.Stop()
	}
	select {
	case <-s.closeCh:
	default:
		close(s.closeCh)
	}
}

func Now() int64 {
	return time.Now().UnixNano()
}

func expireAtToTime(expireAt int64) time.Time {
	if expireAt <= 0 {
		return time.Time{}
	}
	return time.Unix(0, expireAt)
}

// hashBKRD 使用 BKDR 哈希算法计算键的哈希值。
func hashBKRD(key string) (hash int32) {
	for i := 0; i < len(key); i++ {
		hash = hash*131 + int32(key[i])
	}
	return hash
}

// maskOfNextPowOf2 计算大于等于输入值的最近 2 的幂减一，作为桶掩码。
func maskOfNextPowOf2(cap uint16) uint16 {
	if cap > 0 && cap&(cap-1) == 0 {
		return cap - 1
	}
	cap |= cap >> 1
	cap |= cap >> 2
	cap |= cap >> 4
	return cap | (cap >> 8)
}

func Create(cap uint16) *cache {
	return &cache{
		dlnk: make([][2]uint16, cap+1),
		m:    make([]node, cap),
		hmap: make(map[string]uint16, cap),
		last: 0,
	}
}

// p 和 n 分别表示前驱、后继的索引位，保留为包级变量以兼容现有测试。
var p, n = uint16(0), uint16(1)

// put 向缓存中写入节点；若键已存在则更新，否则按 LRU 规则覆盖尾节点。
func (c *cache) put(key string, val Value, expireAt int64, onEvicted func(string, Value)) int {
	if idx, ok := c.hmap[key]; ok {
		c.m[idx-1].v = val
		c.m[idx-1].expireAt = expireAt
		c.adjust(idx, Tail, Head)
		return 0
	}

	if c.last == uint16(cap(c.m)) {
		tailIdx := c.dlnk[0][Tail]
		tail := &c.m[tailIdx-1]
		if onEvicted != nil && tail.expireAt > 0 {
			onEvicted(tail.k, tail.v)
		}
		delete(c.hmap, tail.k)
		c.hmap[key] = tailIdx
		tail.k = key
		tail.v = val
		tail.expireAt = expireAt
		c.adjust(tailIdx, Tail, Head)
		return 1
	}

	c.last++
	if len(c.hmap) == 0 {
		c.dlnk[0][Tail] = c.last
	} else {
		c.dlnk[c.dlnk[0][Head]][p] = c.last
	}

	c.m[c.last-1].k = key
	c.m[c.last-1].v = val
	c.m[c.last-1].expireAt = expireAt
	c.dlnk[c.last] = [2]uint16{0, c.dlnk[0][Head]}
	if c.dlnk[0][Head] != 0 {
		c.dlnk[c.dlnk[0][Head]][p] = c.last
	}
	c.dlnk[0][Head] = c.last
	if c.dlnk[0][Tail] == 0 {
		c.dlnk[0][Tail] = c.last
	}
	c.hmap[key] = c.last
	return 1
}

func (c *cache) get(key string) (*node, int) {
	if idx, ok := c.hmap[key]; ok {
		c.adjust(idx, Tail, Head)
		return &c.m[idx-1], 1
	}
	return nil, 0
}

// del 将节点标记为删除并移动到尾部，便于后续复用存储槽位。
func (c *cache) del(key string) (*node, int, int64) {
	if idx, ok := c.hmap[key]; ok && c.m[idx-1].expireAt > 0 {
		oldExpireAt := c.m[idx-1].expireAt
		c.m[idx-1].expireAt = 0
		c.adjust(idx, Head, Tail)
		return &c.m[idx-1], 1, oldExpireAt
	}
	return nil, 0, 0
}

func (c *cache) walk(walker func(key string, value Value, expireAt int64) bool) {
	for idx := c.dlnk[0][Head]; idx != 0; idx = c.dlnk[idx][n] {
		if c.m[idx-1].expireAt > 0 && !walker(c.m[idx-1].k, c.m[idx-1].v, c.m[idx-1].expireAt) {
			return
		}
	}
}

// adjust 调整节点在链表中的位置，to 为 Head 时移动到头部，否则移动到尾部。
func (c *cache) adjust(idx, from, to uint16) {
	if idx == 0 {
		return
	}

	prev := c.dlnk[idx][p]
	next := c.dlnk[idx][n]

	if prev != 0 {
		c.dlnk[prev][n] = next
	}
	if next != 0 {
		c.dlnk[next][p] = prev
	}

	if c.dlnk[0][Head] == idx {
		c.dlnk[0][Head] = next
	}
	if c.dlnk[0][Tail] == idx {
		c.dlnk[0][Tail] = prev
	}

	if to == Head {
		c.dlnk[idx][p] = 0
		c.dlnk[idx][n] = c.dlnk[0][Head]
		if c.dlnk[0][Head] != 0 {
			c.dlnk[c.dlnk[0][Head]][p] = idx
		}
		c.dlnk[0][Head] = idx
		if c.dlnk[0][Tail] == 0 {
			c.dlnk[0][Tail] = idx
		}
	} else {
		c.dlnk[idx][n] = 0
		c.dlnk[idx][p] = c.dlnk[0][Tail]
		if c.dlnk[0][Tail] != 0 {
			c.dlnk[c.dlnk[0][Tail]][n] = idx
		}
		c.dlnk[0][Tail] = idx
		if c.dlnk[0][Head] == 0 {
			c.dlnk[0][Head] = idx
		}
	}

	_ = from
}

func (s *lru2Store) _get(key string, idx, level int32) (*node, int) {
	n, st := s.caches[idx][level].get(key)
	if st == 0 || n == nil {
		return nil, 0
	}
	if n.expireAt <= 0 || Now() >= n.expireAt {
		s.caches[idx][level].del(key)
		return nil, 0
	}
	return n, st
}

func (s *lru2Store) delete(key string, idx int32) bool {
	n1, s1, _ := s.caches[idx][0].del(key)
	n2, s2, _ := s.caches[idx][1].del(key)
	deleted := s1 > 0 || s2 > 0
	if !deleted {
		return false
	}

	if s.onEvicted != nil {
		if n2 != nil && n2.v != nil {
			s.onEvicted(key, n2.v)
		} else if n1 != nil && n1.v != nil {
			s.onEvicted(key, n1.v)
		}
	}
	return true
}

func (s *lru2Store) cleanupLoop() {
	for {
		select {
		case <-s.closeCh:
			return
		case <-s.cleanupTick.C:
			currentTime := Now()
			for i := range s.caches {
				s.locks[i].Lock()

				var expiredKeys []string
				s.caches[i][0].walk(func(key string, value Value, expireAt int64) bool {
					if expireAt > 0 && currentTime >= expireAt {
						expiredKeys = append(expiredKeys, key)
					}
					return true
				})
				s.caches[i][1].walk(func(key string, value Value, expireAt int64) bool {
					if expireAt > 0 && currentTime >= expireAt {
						for _, existed := range expiredKeys {
							if existed == key {
								return true
							}
						}
						expiredKeys = append(expiredKeys, key)
					}
					return true
				})

				for _, key := range expiredKeys {
					s.delete(key, int32(i))
				}
				s.locks[i].Unlock()
			}
		}
	}
}
