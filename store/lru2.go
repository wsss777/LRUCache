package store

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	Head = 0 // 链表头部（最常使用的节点）
	Tail = 1 // 链表尾部（最久未使用的节点）
)

type lru2Store struct {
	locks       []sync.Mutex
	caches      [][2]*cache
	onEvicted   func(k string, v Value)
	cleanupTick *time.Ticker
	mask        int32
}

type node struct {
	k        string
	v        Value
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

	mask := maskOfNextPowOf2(opts.BucketCount)
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

func (s *lru2Store) Get(key string) (Value, bool) {
	idx := hashBKRD(key) & s.mask
	s.locks[idx].Lock()
	defer s.locks[idx].Unlock()
	currentTime := Now()

	//一级缓存
	n1, status1, expireAt := s.caches[idx][0].del(key)
	if status1 > 0 {
		if n1 == nil { // 防御性检查，虽然当前实现不会发生
			s.delete(key, idx)
			return nil, false
		}
		if expireAt > 0 && currentTime >= expireAt {
			s.delete(key, idx)
			fmt.Println("找到项目已经过期，删除它")
			return nil, false
		}

		//有效，将其移至二级缓存
		s.caches[idx][1].put(key, n1.v, expireAt, s.onEvicted)
		fmt.Println("项目有效，将其移至二级缓存")
		return n1.v, true
	}

	//二级缓存
	n2, status2 := s._get(key, idx, 1)
	if status2 > 0 && n2 != nil {
		if n2.expireAt > 0 && currentTime >= n2.expireAt {
			s.delete(key, idx)
			fmt.Println("找到项目已经过期，删除它")
			return nil, false
		}
		return n2.v, true
	}
	return nil, false
}

func (s *lru2Store) Set(key string, value Value) error {
	return s.SetWithExpiration(key, value, 1e7)
}

func (s *lru2Store) SetWithExpiration(key string, value Value, expiration time.Duration) error {
	expireAt := int64(0)
	if expiration > 0 {
		expireAt = Now() + int64(expiration.Nanoseconds())

	}
	idx := hashBKRD(key) & s.mask
	s.locks[idx].Lock()
	defer s.locks[idx].Unlock()

	s.caches[idx][0].put(key, value, expireAt, s.onEvicted)
	return nil
}

func (s *lru2Store) Delete(key string) bool {
	idx := hashBKRD(key) & s.mask
	s.locks[idx].Lock()
	defer s.locks[idx].Unlock()

	return s.delete(key, idx)
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
			for _, k := range keys {
				if key == k {
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
}

func Now() int64 {
	return atomic.LoadInt64(&clock)
}

func init() {
	go func() {
		for {
			atomic.StoreInt64(&clock, time.Now().UnixNano())
			for i := 0; i < 9; i++ {

				time.Sleep(time.Millisecond * 100)
				atomic.AddInt64(&clock, int64(100*time.Microsecond))

			}
			time.Sleep(time.Millisecond * 100)
		}
	}()
}

// 实现了 BKDR 哈希算法，用于计算键的哈希值
func hashBKRD(key string) (hash int32) {
	for i := 0; i < len(key); i++ {
		hash = hash*131 + int32(key[i])
	}
	return hash
}

// maskOfNextPowOf2 计算大于或等于输入值的最近 2 的幂次方减一作为掩码值
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

// 内部时钟，减少 time.Now() 调用造成的 GC 压力
var clock, p, n = time.Now().UnixNano(), uint16(0), uint16(1)

// 向缓存中添加项，如果是新增返回 1，更新返回 0
func (c *cache) put(key string, val Value, expireAt int64, onEvicted func(string, Value)) int {
	//已经存在
	if idx, ok := c.hmap[key]; ok {
		c.m[idx-1].v, c.m[idx-1].expireAt = val, expireAt
		c.adjust(idx, Tail, Head)
		return 0
	}
	//hmap容量满了
	if c.last == uint16(cap(c.m)) {
		tail := &c.m[c.dlnk[0][Tail]-1]
		if onEvicted != nil && (*tail).expireAt > 0 {
			onEvicted((*tail).k, (*tail).v)
		}
		delete(c.hmap, (*tail).k)
		c.hmap[key], (*tail).k, (*tail).v, (*tail).expireAt = c.dlnk[0][Tail], key, val, expireAt
		c.adjust(c.dlnk[0][Tail], Tail, Head)
		return 1
	}

	c.last++
	if len(c.hmap) <= 0 {
		c.dlnk[0][Tail] = c.last
	} else {
		c.dlnk[c.dlnk[0][Head]][p] = c.last
	}
	// 初始化新节点并更新链表指针
	c.m[c.last-1].k = key
	c.m[c.last-1].v = val
	c.m[c.last-1].expireAt = expireAt
	// 新节点：前驱=0，后继=原头部
	c.dlnk[c.last] = [2]uint16{0, c.dlnk[0][Head]}
	// 原头部的前驱指向新节点
	if c.dlnk[0][Head] != 0 {
		c.dlnk[c.dlnk[0][Head]][p] = c.last
	}
	c.dlnk[0][Head] = c.last

	// 如果是第一个节点，更新尾部
	if c.dlnk[0][Tail] == 0 {
		c.dlnk[0][Tail] = c.last
	}

	c.hmap[key] = c.last

	return 1
}

// 从缓存中获取键对应的节点和状态
func (c *cache) get(key string) (*node, int) {
	if idx, ok := c.hmap[key]; ok {
		c.adjust(idx, Tail, Head)
		return &c.m[idx-1], 1
	}
	return nil, 0
}

// 从缓存中删除键对应的项
func (c *cache) del(key string) (*node, int, int64) {
	if idx, ok := c.hmap[key]; ok && c.m[idx-1].expireAt > 0 {
		e := c.m[idx-1].expireAt
		c.m[idx-1].expireAt = 0   // 标记为已删除
		c.adjust(idx, Head, Tail) // 移动到链表尾部
		return &c.m[idx-1], 1, e
	}

	return nil, 0, 0
}

// 遍历缓存中的所有有效项
func (c *cache) walk(walker func(key string, value Value, expireAt int64) bool) {
	for idx := c.dlnk[0][Head]; idx != 0; idx = c.dlnk[idx][n] {
		if c.m[idx-1].expireAt > 0 && !walker(c.m[idx-1].k, c.m[idx-1].v, c.m[idx-1].expireAt) {
			return
		}
	}
}

// 调整节点在链表中的位置
// 当 f=1, t=0 时，移动到链表头部；否则移动到链表尾部
func (c *cache) adjust(idx, f, t uint16) {
	if idx == 0 {
		return
	}
	prev := c.dlnk[idx][p] // 原前驱
	next := c.dlnk[idx][n] // 原后继
	// 从当前位置摘除
	if prev != 0 {
		c.dlnk[prev][n] = next // 前驱的后继指向原后继
	}
	if next != 0 {
		c.dlnk[next][p] = prev // 后继的前驱指向原前驱
	}

	//如果原来是头部或尾部，更新哨兵
	if c.dlnk[0][Head] == idx {
		c.dlnk[0][Head] = next
	}
	if c.dlnk[0][Tail] == idx {
		c.dlnk[0][Tail] = prev
	}
	//插入到目标位置（头部或尾部）
	if t == Head {
		// 插入到头部
		c.dlnk[idx][p] = 0
		c.dlnk[idx][n] = c.dlnk[0][Head]
		if c.dlnk[0][Head] != 0 {
			c.dlnk[c.dlnk[0][Head]][p] = idx
		}
		c.dlnk[0][Head] = idx
	} else {
		// 插入到尾部
		c.dlnk[idx][n] = 0
		c.dlnk[idx][p] = c.dlnk[0][Tail]
		if c.dlnk[0][Tail] != 0 {
			c.dlnk[c.dlnk[0][Tail]][n] = idx
		}
		c.dlnk[0][Tail] = idx
	}

}
func (s *lru2Store) _get(key string, idx, level int32) (*node, int) {
	if n, st := s.caches[idx][level].get(key); st > 0 && n != nil {
		currentTime := Now()
		if n.expireAt <= 0 || currentTime >= n.expireAt {
			// 过期或已删除
			return nil, 0
		}
		return n, st
	}

	return nil, 0
}
func (s *lru2Store) delete(key string, idx int32) bool {
	n1, s1, _ := s.caches[idx][0].del(key)
	n2, s2, _ := s.caches[idx][1].del(key)
	deleted := s1 > 0 || s2 > 0

	if deleted && s.onEvicted != nil {
		if n1 != nil && n1.v != nil {
			s.onEvicted(key, n1.v)
		} else if n2 != nil && n2.v != nil {
			s.onEvicted(key, n2.v)
		}
	}

	if deleted {
		//s.expirations.Delete(key)
	}

	return deleted
}
func (s *lru2Store) cleanupLoop() {
	for range s.cleanupTick.C {
		currentTime := Now()

		for i := range s.caches {
			s.locks[i].Lock()

			// 检查并清理过期项目
			var expiredKeys []string

			s.caches[i][0].walk(func(key string, value Value, expireAt int64) bool {
				if expireAt > 0 && currentTime >= expireAt {
					expiredKeys = append(expiredKeys, key)
				}
				return true
			})

			s.caches[i][1].walk(func(key string, value Value, expireAt int64) bool {
				if expireAt > 0 && currentTime >= expireAt {
					for _, k := range expiredKeys {
						if key == k {
							// 避免重复
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
