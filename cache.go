package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const cacheDir = ".cache"

// cachedFetch returns the cached raw response body for (provider, key) if
// present, otherwise calls fetch and caches a successful result. This
// exists so that reshaping what we parse out of a provider's response
// (e.g. adding fields we previously discarded) never requires paying for
// the same lookup twice.
func cachedFetch(ctx context.Context, provider, key string, fetch func() ([]byte, error)) ([]byte, error) {
	path := cachePath(provider, key)
	if data, err := os.ReadFile(path); err == nil {
		slog.DebugContext(ctx, "cache hit", "provider", provider, "req_id", reqID(ctx))
		return data, nil
	}

	start := time.Now()
	data, err := fetch()
	if err != nil {
		slog.WarnContext(ctx, "provider call failed", "provider", provider, "req_id", reqID(ctx),
			"elapsed_ms", time.Since(start).Milliseconds(), "err", err)
		return nil, err
	}
	slog.DebugContext(ctx, "provider call ok", "provider", provider, "req_id", reqID(ctx),
		"elapsed_ms", time.Since(start).Milliseconds())

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
	return data, nil
}

func cachePath(provider, key string) string {
	sum := sha1.Sum([]byte(key))
	return filepath.Join(cacheDir, provider, hex.EncodeToString(sum[:])+".json")
}
