package pool

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// Cache persists the slow-to-compute parts of the pool's state between runs
// so the first frame can show last-known values while fresh ones load.
type Cache struct {
	Statuses map[string]Status `json:"statuses"`
	Live     map[string]int    `json:"live"`
}

func (r *Repo) cachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	sum := sha1.Sum([]byte(r.Main))
	return filepath.Join(dir, "dv", "state-"+hex.EncodeToString(sum[:6])+".json")
}

// LoadCache returns the persisted state, or an empty cache.
func (r *Repo) LoadCache() Cache {
	c := Cache{Statuses: map[string]Status{}, Live: map[string]int{}}
	path := r.cachePath()
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	if c.Statuses == nil {
		c.Statuses = map[string]Status{}
	}
	if c.Live == nil {
		c.Live = map[string]int{}
	}
	return c
}

// SaveCache persists c. Failures are ignored: the cache is an optimization.
func (r *Repo) SaveCache(c Cache) {
	path := r.cachePath()
	if path == "" {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = writeFileAtomic(path, data, 0o644)
}
