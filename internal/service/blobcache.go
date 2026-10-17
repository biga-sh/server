package service

import (
	"container/list"
	"strconv"
	"sync"
	"time"

	"github.com/biga-sh/server/internal/store"
)

// blobCache is a byte-bounded LRU of file contents. Blobs are immutable
// once written, so entries never go stale; keys include the session's
// creation time so a deleted session's content is never served to a new
// session that reuses its ID.
type blobCache struct {
	mu    sync.Mutex
	max   int64
	size  int64
	order *list.List
	items map[string]*list.Element
}

type cacheEntry struct {
	key  string
	blob store.Blob
}

func newBlobCache(maxBytes int64) *blobCache {
	return &blobCache{max: maxBytes, order: list.New(), items: make(map[string]*list.Element)}
}

func blobKey(sessionID string, created time.Time, hash string) string {
	return sessionID + "\x00" + strconv.FormatInt(created.UnixNano(), 36) + "\x00" + hash
}

func (c *blobCache) get(key string) (store.Blob, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return store.Blob{}, false
	}
	c.order.MoveToFront(el)
	e, _ := el.Value.(*cacheEntry)
	return e.blob, true
}

func (c *blobCache) put(key string, b store.Blob) {
	cost := int64(len(b.Content) + len(key))
	// One huge blob must not evict everything else.
	if c.max <= 0 || cost > c.max/8 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; ok {
		return
	}
	c.items[key] = c.order.PushFront(&cacheEntry{key: key, blob: b})
	c.size += cost
	for c.size > c.max {
		last := c.order.Back()
		e, _ := last.Value.(*cacheEntry)
		c.order.Remove(last)
		delete(c.items, e.key)
		c.size -= int64(len(e.blob.Content) + len(e.key))
	}
}
