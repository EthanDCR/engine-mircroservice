package main

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
)

const cacheDir = ".cache"

// cachedFetch returns the cached raw response body for (provider, key) if
// present, otherwise calls fetch and caches a successful result. This
// exists so that reshaping what we parse out of a provider's response
// (e.g. adding fields we previously discarded) never requires paying for
// the same lookup twice.
func cachedFetch(provider, key string, fetch func() ([]byte, error)) ([]byte, error) {
	path := cachePath(provider, key)
	if data, err := os.ReadFile(path); err == nil {
		return data, nil
	}

	data, err := fetch()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
	return data, nil
}

func cachePath(provider, key string) string {
	sum := sha1.Sum([]byte(key))
	return filepath.Join(cacheDir, provider, hex.EncodeToString(sum[:])+".json")
}
