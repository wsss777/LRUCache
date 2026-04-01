package store

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 测试值类型是测试里使用的简单 Value 实现。
type testValue string

func (v testValue) Len() int {
	return len(v)
}

func TestCacheBasic(t *testing.T) {
	c := Create(5)

	if c == nil {
		t.Fatal("创建底层缓存失败")
	}
	if c.last != 0 {
		t.Fatalf("初始 last 应为 0，实际为 %d", c.last)
	}

	status := c.put("key1", testValue("value1"), Now()+int64(time.Hour), nil)
	if status != 1 {
		t.Fatalf("新增节点应返回 1，实际为 %d", status)
	}

	n, ok := c.get("key1")
	if ok != 1 || n == nil {
		t.Fatal("应能取回刚写入的节点")
	}
	if n.k != "key1" || n.v != testValue("value1") {
		t.Fatalf("节点内容不正确: %+v", n)
	}

	n, status, _ = c.del("key1")
	if status != 1 || n == nil {
		t.Fatal("删除现有节点应成功")
	}
	if n.expireAt != 0 {
		t.Fatalf("删除后节点应被标记为 tombstone，expireAt=%d", n.expireAt)
	}

	n, ok = c.get("key1")
	if ok != 1 || n == nil || n.expireAt != 0 {
		t.Fatal("删除后的节点槽位仍应存在，但必须是 tombstone")
	}
}

func TestCacheLRUEviction(t *testing.T) {
	var evictedKeys []string
	c := Create(3)

	onEvicted := func(key string, value Value) {
		evictedKeys = append(evictedKeys, key)
	}

	c.put("key1", testValue("value1"), Now()+int64(time.Hour), onEvicted)
	c.put("key2", testValue("value2"), Now()+int64(time.Hour), onEvicted)
	c.put("key3", testValue("value3"), Now()+int64(time.Hour), onEvicted)

	// 访问 key1，使 key2 成为最久未使用节点。
	c.get("key1")
	c.put("key4", testValue("value4"), Now()+int64(time.Hour), onEvicted)

	if len(evictedKeys) != 1 || evictedKeys[0] != "key2" {
		t.Fatalf("应淘汰 key2，实际淘汰 %v", evictedKeys)
	}

	n, ok := c.get("key2")
	if ok != 0 || n != nil {
		t.Fatal("key2 应已被淘汰")
	}
}

func TestLRU2StoreBasicOperations(t *testing.T) {
	store := newLRU2Cache(Options{
		BucketCount:     4,
		CapPerBucket:    2,
		Level2Cap:       2,
		CleanupInterval: time.Minute,
	})
	defer store.Close()

	if err := store.Set("key1", testValue("value1")); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}

	value, found := store.Get("key1")
	if !found || value != testValue("value1") {
		t.Fatalf("Get 失败，value=%v found=%v", value, found)
	}

	if err := store.Set("key1", testValue("value1-updated")); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	value, found = store.Get("key1")
	if !found || value != testValue("value1-updated") {
		t.Fatalf("更新后读取失败，value=%v found=%v", value, found)
	}

	if !store.Delete("key1") {
		t.Fatal("删除现有键应成功")
	}

	if _, found = store.Get("key1"); found {
		t.Fatal("删除后不应再命中")
	}
}

func TestLRU2StoreHotPromotion(t *testing.T) {
	store := newLRU2Cache(Options{
		BucketCount:     1,
		CapPerBucket:    2,
		Level2Cap:       2,
		CleanupInterval: time.Minute,
	})
	defer store.Close()

	store.Set("key1", testValue("value1"))
	store.Set("key2", testValue("value2"))

	// 首次访问把 key1 从一级缓存提升到二级缓存。
	value, found := store.Get("key1")
	if !found || value != testValue("value1") {
		t.Fatalf("key1 应在访问后晋升到热点层，value=%v found=%v", value, found)
	}

	// 后续写入冷数据只应影响一级缓存，不应挤掉热点 key1。
	store.Set("key3", testValue("value3"))
	store.Set("key4", testValue("value4"))
	value, found = store.Get("key1")
	if !found || value != testValue("value1") {
		t.Fatalf("热点 key1 应仍在二级缓存中，value=%v found=%v", value, found)
	}

	// 再把 key3、key4 提升为热点，二级缓存满后最老热点 key1 应被淘汰。
	if _, found = store.Get("key3"); !found {
		t.Fatal("key3 应可读并被提升为热点")
	}
	if _, found = store.Get("key4"); !found {
		t.Fatal("key4 应可读并被提升为热点")
	}
	if _, found = store.Get("key1"); found {
		t.Fatal("更晚产生的热点占满二级缓存后，key1 应被淘汰")
	}
}

func TestLRU2StoreExpiration(t *testing.T) {
	store := newLRU2Cache(Options{
		BucketCount:     1,
		CapPerBucket:    5,
		Level2Cap:       5,
		CleanupInterval: 100 * time.Millisecond,
	})
	defer store.Close()

	store.SetWithExpiration("expires-soon", testValue("value"), 200*time.Millisecond)
	store.SetWithExpiration("expires-later", testValue("value"), time.Hour)

	if _, found := store.Get("expires-soon"); !found {
		t.Fatal("短期键初始阶段应可命中")
	}
	if _, found := store.Get("expires-later"); !found {
		t.Fatal("长期键初始阶段应可命中")
	}

	time.Sleep(300 * time.Millisecond)

	if _, found := store.Get("expires-soon"); found {
		t.Fatal("短期键应已过期")
	}
	if _, found := store.Get("expires-later"); !found {
		t.Fatal("长期键不应过期")
	}
}

func TestLRU2StoreCleanupLoop(t *testing.T) {
	store := newLRU2Cache(Options{
		BucketCount:     1,
		CapPerBucket:    5,
		Level2Cap:       5,
		CleanupInterval: 100 * time.Millisecond,
	})
	defer store.Close()

	store.SetWithExpiration("expires1", testValue("value1"), 200*time.Millisecond)
	store.SetWithExpiration("expires2", testValue("value2"), 200*time.Millisecond)
	store.SetWithExpiration("keeps", testValue("value3"), time.Hour)

	time.Sleep(500 * time.Millisecond)

	if _, found := store.Get("expires1"); found {
		t.Fatal("expires1 应已被清理")
	}
	if _, found := store.Get("expires2"); found {
		t.Fatal("expires2 应已被清理")
	}
	if _, found := store.Get("keeps"); !found {
		t.Fatal("keeps 不应被清理")
	}
}

func TestLRU2StoreClear(t *testing.T) {
	store := newLRU2Cache(Options{
		BucketCount:     2,
		CapPerBucket:    5,
		Level2Cap:       5,
		CleanupInterval: time.Minute,
	})
	defer store.Close()

	for i := 0; i < 10; i++ {
		store.Set(fmt.Sprintf("key%d", i), testValue(fmt.Sprintf("value%d", i)))
	}
	if store.Len() == 0 {
		t.Fatal("写入后长度不应为 0")
	}

	store.Clear()

	if store.Len() != 0 {
		t.Fatalf("清空后长度应为 0，实际为 %d", store.Len())
	}
}

func TestLRU2Store_Get(t *testing.T) {
	store := newLRU2Cache(Options{
		BucketCount:     1,
		CapPerBucket:    5,
		Level2Cap:       5,
		CleanupInterval: time.Minute,
	})
	defer store.Close()

	idx := hashBKRD("test-key") & store.mask
	store.caches[idx][0].put("test-key", testValue("test-value"), Now()+int64(time.Hour), nil)
	store.caches[idx][1].put("test-key2", testValue("test-value2"), Now()+int64(time.Hour), nil)

	n, status := store._get("test-key", idx, 0)
	if status != 1 || n == nil || n.v != testValue("test-value") {
		t.Fatal("_get 应能从一级缓存取回数据")
	}

	n, status = store._get("test-key2", idx, 1)
	if status != 1 || n == nil || n.v != testValue("test-value2") {
		t.Fatal("_get 应能从二级缓存取回数据")
	}

	store.caches[idx][0].put("expired", testValue("value"), Now()-int64(time.Second), nil)
	n, status = store._get("expired", idx, 0)
	if status != 0 || n != nil {
		t.Fatal("_get 不应返回已过期数据")
	}
}

func TestLRU2StoreDelete(t *testing.T) {
	var evictedKeys []string
	store := newLRU2Cache(Options{
		BucketCount:     1,
		CapPerBucket:    5,
		Level2Cap:       5,
		CleanupInterval: time.Minute,
		OnEvicted: func(key string, value Value) {
			evictedKeys = append(evictedKeys, key)
		},
	})
	defer store.Close()

	idx := hashBKRD("test-key") & store.mask
	store.caches[idx][0].put("test-key", testValue("test-value"), Now()+int64(time.Hour), nil)
	store.caches[idx][1].put("test-key2", testValue("test-value2"), Now()+int64(time.Hour), nil)

	if !store.delete("test-key", idx) {
		t.Fatal("删除一级缓存中的键应成功")
	}
	if len(evictedKeys) != 1 || evictedKeys[0] != "test-key" {
		t.Fatalf("一级缓存删除应触发回调，实际 %v", evictedKeys)
	}

	evictedKeys = nil
	if !store.delete("test-key2", idx) {
		t.Fatal("删除二级缓存中的键应成功")
	}
	if len(evictedKeys) != 1 || evictedKeys[0] != "test-key2" {
		t.Fatalf("二级缓存删除应触发回调，实际 %v", evictedKeys)
	}
}

func TestLRU2StoreConcurrent(t *testing.T) {
	store := newLRU2Cache(Options{
		BucketCount:     8,
		CapPerBucket:    100,
		Level2Cap:       200,
		CleanupInterval: time.Minute,
	})
	defer store.Close()

	const goroutines = 10
	const operationsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			prefix := fmt.Sprintf("g%d-", id)

			for i := 0; i < operationsPerGoroutine; i++ {
				key := prefix + strconv.Itoa(i)
				value := testValue(fmt.Sprintf("value-%s", key))
				if err := store.Set(key, value); err != nil {
					t.Errorf("Set 失败: %v", err)
				}
			}

			for i := 0; i < operationsPerGoroutine; i++ {
				key := prefix + strconv.Itoa(i)
				expectedValue := testValue(fmt.Sprintf("value-%s", key))
				value, found := store.Get(key)
				if !found {
					t.Errorf("Get 失败: %s", key)
				} else if value != expectedValue {
					t.Errorf("Get 值错误: key=%s expected=%s got=%v", key, expectedValue, value)
				}
			}

			for i := 0; i < operationsPerGoroutine/2; i++ {
				key := prefix + strconv.Itoa(i)
				if !store.Delete(key) {
					t.Errorf("Delete 失败: %s", key)
				}
			}
		}(g)
	}

	wg.Wait()

	expectedItems := goroutines * operationsPerGoroutine / 2
	actualItems := store.Len()
	tolerance := expectedItems / 10
	if actualItems < expectedItems-tolerance || actualItems > expectedItems+tolerance {
		t.Fatalf("剩余项数偏差过大，expected≈%d actual=%d", expectedItems, actualItems)
	}
}

func TestLRU2StoreHitRatio(t *testing.T) {
	store := newLRU2Cache(Options{
		BucketCount:     4,
		CapPerBucket:    10,
		Level2Cap:       20,
		CleanupInterval: time.Minute,
	})
	defer store.Close()

	for i := 0; i < 50; i++ {
		store.Set(fmt.Sprintf("key%d", i), testValue(fmt.Sprintf("value%d", i)))
	}

	hits := 0
	attempts := 0
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("key%d", i)
		_, found := store.Get(key)
		attempts++
		if found {
			hits++
		}
	}

	hitRatio := float64(hits) / float64(attempts)
	if hitRatio < 0.35 || hitRatio > 0.45 {
		t.Fatalf("命中率超出预期区间，ratio=%.2f", hitRatio)
	}
}
