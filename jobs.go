package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type jobStatus string

const (
	jobQueued     jobStatus = "queued"
	jobProcessing jobStatus = "processing"
	jobDone       jobStatus = "done"
	jobFailed     jobStatus = "failed"
)

// job tracks one async enrichment request submitted to the HTTP server.
// Results are held in memory for the life of the process — there's no
// persistence across restarts, which is fine for a low-volume internal
// service but worth revisiting if job volume or uptime grows.
type job struct {
	mu        sync.Mutex
	status    jobStatus
	processed int
	total     int
	err       string

	enrichedCSV []byte
	cleanedCSV  []byte
	createdAt   time.Time
}

func (j *job) setStatus(s jobStatus) {
	j.mu.Lock()
	j.status = s
	j.mu.Unlock()
}

func (j *job) setProgress(done, total int) {
	j.mu.Lock()
	j.processed = done
	j.total = total
	j.mu.Unlock()
}

func (j *job) fail(err error) {
	j.mu.Lock()
	j.status = jobFailed
	j.err = err.Error()
	j.mu.Unlock()
}

func (j *job) finish(enriched, cleaned []byte) {
	j.mu.Lock()
	j.status = jobDone
	j.enrichedCSV = enriched
	j.cleanedCSV = cleaned
	j.mu.Unlock()
}

func (j *job) snapshot() (status jobStatus, processed, total int, errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status, j.processed, j.total, j.err
}

type jobStore struct {
	mu   sync.Mutex
	jobs map[string]*job
}

func newJobStore() *jobStore {
	return &jobStore{jobs: make(map[string]*job)}
}

func (s *jobStore) create() (string, *job) {
	id := newJobID()
	j := &job{status: jobQueued, createdAt: time.Now()}
	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()
	return id, j
}

func (s *jobStore) get(id string) (*job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	return j, ok
}

func newJobID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
