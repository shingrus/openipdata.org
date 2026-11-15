package main

// A small cache of drawn maps, in this process only: the same cities and
// countries are asked for again and again. Least recently used entries go
// first. Nothing here is shared between processes and nothing is kept on disk.

import (
	"container/list"
	"html/template"
	"sync"
)

// mapCacheSize is the number of maps kept: at 2 to 8 KB a map, about 5 MB
// in practice and under 10 MB at worst.
const mapCacheSize = 1024

// mapKey names one map: a city (its placed coordinates) or a country (its code).
type mapKey struct {
	lat, lon float64
	code     string
	city     bool
}

type mapEntry struct {
	key mapKey
	svg template.HTML
}

type mapCache struct {
	mu    sync.Mutex
	max   int
	order *list.List // front = used last
	items map[mapKey]*list.Element
}

func newMapCache(max int) *mapCache {
	return &mapCache{max: max, order: list.New(), items: make(map[mapKey]*list.Element, max)}
}

// get returns the map of k, drawing it with draw the first time. An empty
// result (no map for this place) is kept too, so it is not drawn again.
func (c *mapCache) get(k mapKey, draw func() template.HTML) template.HTML {
	c.mu.Lock()
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		svg := e.Value.(*mapEntry).svg
		c.mu.Unlock()
		return svg
	}
	c.mu.Unlock()
	svg := draw() // outside the lock: two requests may draw the same map once each
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*mapEntry).svg
	}
	c.items[k] = c.order.PushFront(&mapEntry{k, svg})
	if c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*mapEntry).key)
	}
	return svg
}

func (c *mapCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
