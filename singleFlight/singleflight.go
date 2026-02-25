package singleFlight

import "sync"

// 代表正在进行或已结束的请求
type call struct {
	wg  sync.WaitGroup
	val interface{}
	err error
}

type Group struct {
	m sync.Map // 使用sync.Map来优化并发性能
}

// Do 针对相同的key，保证多次调用Do()，都只会调用一次fn
func (g *Group) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	if existing, ok := g.m.Load(key); ok {
		c := existing.(*call)
		c.wg.Wait()
		return c.val, c.err
	}

	c := &call{}
	c.wg.Add(1)
	g.m.Store(key, c)

	c.val, c.err = fn()
	c.wg.Done()

	g.m.Delete(key)
	return c.val, c.err
}
