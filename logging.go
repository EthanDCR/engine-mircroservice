package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
)

// initLogging installs a text-handler slog logger as the process default.
// LOG_LEVEL controls verbosity (debug/info/warn/error; default info) — set
// LOG_LEVEL=debug to see cache hits/misses and per-attempt provider calls,
// which are otherwise too noisy for normal operation.
func initLogging() {
	level := slog.LevelInfo
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}

type reqIDKey struct{}

// withReqID attaches a short id to ctx so every log line touched by a
// single incoming request — the HTTP handler, the job goroutine, and each
// DealMachine/BatchData/StormPull call it fans out to — can be grepped
// together by req_id.
func withReqID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey{}, id)
}

func reqID(ctx context.Context) string {
	if v, ok := ctx.Value(reqIDKey{}).(string); ok {
		return v
	}
	return "-"
}

func newReqID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
