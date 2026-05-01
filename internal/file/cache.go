package file

import (
	"container/list"
	"sync"

	"github.com/fnshare/fnshare/internal/crypto"
)

// StripeCache is a byte-bounded LRU of decrypted stripe plaintexts shared
// across all open Readers. Keyed by (file_id, stripe_index).
//
// Bounded by total bytes (default 1 GiB), NOT by entry count — important
// for media files where one 4 MiB stripe and one 4 KiB one have very
// different costs. When inserting a new stripe would exceed the budget,
// we evict from the back of the LRU until it fits.
type StripeCache struct {
	maxBytes int64

	mu       sync.Mutex
	lru      *list.List // front = most recently used
	index    map[string]*list.Element
	curBytes int64
}

type cacheEntry struct {
	key   string
	bytes []byte
}

func NewStripeCache(maxBytes int64) *StripeCache {
	return &StripeCache{
		maxBytes: maxBytes,
		lru:      list.New(),
		index:    map[string]*list.Element{},
	}
}

func cacheKey(fileID string, stripeIdx int) string {
	// fileID is hex; small int → ascii. Avoid allocating with Sprintf.
	return fileID + ":" + itoa(stripeIdx)
}

func (c *StripeCache) Get(fileID string, stripeIdx int) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.index[cacheKey(fileID, stripeIdx)]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(elem)
	return elem.Value.(*cacheEntry).bytes, true
}

func (c *StripeCache) Put(fileID string, stripeIdx int, bytes []byte) {
	if int64(len(bytes)) > c.maxBytes {
		// One stripe larger than the whole budget → don't bother caching.
		return
	}
	k := cacheKey(fileID, stripeIdx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.index[k]; ok {
		// Replace existing.
		old := elem.Value.(*cacheEntry)
		c.curBytes -= int64(len(old.bytes))
		old.bytes = bytes
		c.curBytes += int64(len(bytes))
		c.lru.MoveToFront(elem)
	} else {
		entry := &cacheEntry{key: k, bytes: bytes}
		elem := c.lru.PushFront(entry)
		c.index[k] = elem
		c.curBytes += int64(len(bytes))
	}
	c.evictLocked()
}

func (c *StripeCache) evictLocked() {
	for c.curBytes > c.maxBytes {
		oldest := c.lru.Back()
		if oldest == nil {
			return
		}
		entry := oldest.Value.(*cacheEntry)
		c.lru.Remove(oldest)
		delete(c.index, entry.key)
		c.curBytes -= int64(len(entry.bytes))
	}
}

// Stats returns the current byte usage and entry count.
func (c *StripeCache) Stats() (bytes int64, entries int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curBytes, c.lru.Len()
}

// itoa avoids importing strconv just for one int conversion in a hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// cryptoOpenAES is referenced by reader.go. Implemented here so reader
// doesn't need to import crypto directly (keeps the import graph tight).
func cryptoOpenAES(key, ct []byte) ([]byte, error) {
	return crypto.OpenAES(key, ct)
}
