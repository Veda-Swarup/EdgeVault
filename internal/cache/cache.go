// Package cache implements a byte-bounded disk LRU for chunk payloads.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

var ErrMiss = errors.New("cache miss")

type entry struct {
	key, path string
	size      int64
	dirty     bool
}
type Cache struct {
	mu        sync.Mutex
	dir       string
	max, used int64
	items     map[string]*list.Element
	lru       *list.List
}

func Open(dir string, maxBytes int64) (*Cache, error) {
	if maxBytes <= 0 {
		return nil, errors.New("cache max bytes must be positive")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &Cache{dir: dir, max: maxBytes, items: make(map[string]*list.Element), lru: list.New()}, nil
}

func (c *Cache) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:]))
}

func (c *Cache) Get(key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.items[key]
	if e == nil {
		return nil, ErrMiss
	}
	b, err := os.ReadFile(e.Value.(*entry).path)
	if err != nil {
		delete(c.items, key)
		c.lru.Remove(e)
		c.used -= e.Value.(*entry).size
		return nil, ErrMiss
	}
	c.lru.MoveToFront(e)
	return b, nil
}

// Put writes and fsyncs an entry before publishing it. Dirty entries stay pinned.
func (c *Cache) Put(key string, data []byte, dirty bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if int64(len(data)) > c.max {
		return errors.New("cache entry exceeds capacity")
	}
	if old := c.items[key]; old != nil {
		ent := old.Value.(*entry)
		if ent.dirty && !dirty {
			dirty = true
		}
		c.used -= ent.size
		c.lru.Remove(old)
		delete(c.items, key)
	}
	for c.used+int64(len(data)) > c.max {
		var victim *list.Element
		for e := c.lru.Back(); e != nil; e = e.Prev() {
			if !e.Value.(*entry).dirty {
				victim = e
				break
			}
		}
		if victim == nil {
			return errors.New("cache full of pinned dirty entries")
		}
		ent := victim.Value.(*entry)
		_ = os.Remove(ent.path)
		c.used -= ent.size
		delete(c.items, ent.key)
		c.lru.Remove(victim)
	}
	path := c.path(key)
	f, err := os.CreateTemp(c.dir, ".chunk-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	d, err := os.Open(c.dir)
	if err == nil {
		err = d.Sync()
		_ = d.Close()
	}
	if err != nil {
		return err
	}
	ent := &entry{key: key, path: path, size: int64(len(data)), dirty: dirty}
	c.items[key] = c.lru.PushFront(ent)
	c.used += ent.size
	return nil
}

func (c *Cache) MarkClean(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.items[key]; e != nil {
		e.Value.(*entry).dirty = false
	}
}
func (c *Cache) Stats() (used int64, entries int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used, len(c.items)
}
