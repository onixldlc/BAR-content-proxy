package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is the sidecar metadata stored next to every cached body.
type Entry struct {
	URL         string    `json:"url"`
	Status      int       `json:"status"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	ETag        string    `json:"etag"`
	FetchedAt   time.Time `json:"fetched_at"`
	Immutable   bool      `json:"immutable"`
}

type Cache struct {
	dir      string
	maxBytes int64
	ttl      time.Duration

	mu    sync.Mutex
	hits  int64
	miss  int64
	bytes int64
}

func NewCache(dir string, maxBytes int64, ttl time.Duration) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Cache{dir: dir, maxBytes: maxBytes, ttl: ttl}, nil
}

func cacheKey(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

func (c *Cache) paths(key string) (body, meta string) {
	d := filepath.Join(c.dir, key[:2])
	return filepath.Join(d, key), filepath.Join(d, key+".meta")
}

// Get returns the metadata and an open body file, or nil if the entry is
// absent, corrupt, or stale. Caller closes the file.
func (c *Cache) Get(url string) (*Entry, *os.File) {
	key := cacheKey(url)
	bodyPath, metaPath := c.paths(key)

	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, nil
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, nil
	}
	if !e.Immutable && c.ttl > 0 && time.Since(e.FetchedAt) > c.ttl {
		return nil, nil
	}

	f, err := os.Open(bodyPath)
	if err != nil {
		return nil, nil
	}
	st, err := f.Stat()
	if err != nil || st.Size() != e.Size {
		// Body and metadata disagree; treat as a miss rather than serving
		// a truncated archive, which pr-downloader would reject anyway.
		f.Close()
		return nil, nil
	}
	c.mu.Lock()
	c.hits++
	c.mu.Unlock()
	return &e, f
}

func (c *Cache) MarkMiss() {
	c.mu.Lock()
	c.miss++
	c.mu.Unlock()
}

// NewTemp creates the scratch file a fetch writes into.
func (c *Cache) NewTemp(url string) (*os.File, error) {
	key := cacheKey(url)
	d := filepath.Join(c.dir, key[:2])
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, err
	}
	return os.CreateTemp(d, key+".part-*")
}

// Commit atomically publishes a completed temp file as the cache body.
//
// The file is deliberately left open: clients may still be streaming from
// it, and on Linux the rename does not disturb their descriptors. Closing
// is the partialFile's job once the last reader goes away.
func (c *Cache) Commit(tmp *os.File, e *Entry) error {
	key := cacheKey(e.URL)
	bodyPath, metaPath := c.paths(key)

	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), bodyPath); err != nil {
		return err
	}

	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.WriteFile(metaPath, raw, 0o644); err != nil {
		return err
	}

	c.mu.Lock()
	c.bytes += e.Size
	c.mu.Unlock()
	return nil
}

// Discard unlinks a failed download. The descriptor stays open so readers
// still blocked on it can drain and see the error.
func (c *Cache) Discard(tmp *os.File) {
	os.Remove(tmp.Name())
}

func (c *Cache) Stats() (hits, miss int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.miss
}

type cachedFile struct {
	path    string
	metaTo  string
	size    int64
	modTime time.Time
}

// Sweep enforces CacheMaxBytes by deleting least-recently-modified bodies
// until the cache is back under the limit. No-op when the limit is 0.
func (c *Cache) Sweep() {
	if c.maxBytes <= 0 {
		return
	}

	var files []cachedFile
	var total int64

	err := filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".meta") || strings.Contains(name, ".part-") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		files = append(files, cachedFile{
			path:    path,
			metaTo:  path + ".meta",
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("cache: sweep walk: %v", err)
	}

	if total <= c.maxBytes {
		return
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	freed := int64(0)
	for _, f := range files {
		if total-freed <= c.maxBytes {
			break
		}
		os.Remove(f.path)
		os.Remove(f.metaTo)
		freed += f.size
	}
	log.Printf("cache: swept %d bytes (was %d, limit %d)", freed, total, c.maxBytes)
}

func (c *Cache) SweepLoop(every time.Duration) {
	c.Sweep()
	t := time.NewTicker(every)
	for range t.C {
		c.Sweep()
	}
}
