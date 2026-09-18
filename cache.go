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
		// Full raw body, not just "cache hit" — a cached response is still
		// what this request actually used, and our structs only parse out a
		// fraction of what providers return (owner_occupied went unparsed
		// for months this way). Logged at Info, not Debug, so it's visible
		// without setting LOG_LEVEL=debug — the whole point is to be able to
		// check this after the fact, not to have turned on verbose logging
		// in advance of a problem.
		slog.InfoContext(ctx, "cache hit", "provider", provider, "req_id", reqID(ctx),
			"raw_response", string(data))
		return data, nil
	}

	start := time.Now()
	data, err := fetch()
	if err != nil {
		slog.WarnContext(ctx, "provider call failed", "provider", provider, "req_id", reqID(ctx),
			"elapsed_ms", time.Since(start).Milliseconds(), "err", err)
		return nil, err
	}
	slog.InfoContext(ctx, "provider call ok", "provider", provider, "req_id", reqID(ctx),
		"elapsed_ms", time.Since(start).Milliseconds(), "raw_response", string(data))

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
	return data, nil
}

func cachePath(provider, key string) string {
	sum := sha1.Sum([]byte(key))
	return filepath.Join(cacheDir, provider, hex.EncodeToString(sum[:])+".json")
}
