package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
)

// runServe starts the HTTP microservice: submit a CSV, poll a job for
// progress, fetch the finished CSV once done. Requests never block on the
// enrichment run itself — DealMachine alone is rate-limited to 2.5 req/sec
// shared across all rows, so any real batch takes longer than a typical
// serverless caller (e.g. a base44 function) would be willing to hold a
// connection open for.
func runServe(addr string, c *clients) {
	apiKey := os.Getenv("ENGINE_API_KEY")
	if apiKey == "" {
		log.Fatal("ENGINE_API_KEY must be set to run in serve mode")
	}

	store := newJobStore()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /enrich", requireAPIKey(apiKey, handleEnrich(c, store)))
	mux.HandleFunc("GET /jobs/{id}", requireAPIKey(apiKey, handleJobStatus(store)))
	mux.HandleFunc("GET /jobs/{id}/result", requireAPIKey(apiKey, handleJobResult(store)))
	mux.HandleFunc("POST /enrich-one", requireAPIKey(apiKey, handleEnrichOne(c)))

	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func requireAPIKey(key string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != key {
			http.Error(w, "invalid or missing X-API-Key", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleEnrich accepts a raw CSV body, validates it has the required
// columns, and kicks off processing in the background, returning a job id
// immediately.
func handleEnrich(c *clients, store *jobStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		header, rows, err := readCSVFrom(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("reading CSV body: %v", err), http.StatusBadRequest)
			return
		}
		if _, err := resolveColumns(header); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		workers := 5
		if v := r.URL.Query().Get("workers"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				workers = n
			}
		}

		id, j := store.create()
		j.setProgress(0, len(rows))

		go func() {
			j.setStatus(jobProcessing)

			fullHeader, fullRows, err := enrichCSV(context.Background(), c, header, rows, workers, j.setProgress)
			if err != nil {
				j.fail(err)
				return
			}
			enrichedCSV, err := encodeCSV(fullHeader, fullRows)
			if err != nil {
				j.fail(err)
				return
			}
			cleanHeader, cleanRows := cleanForReps(fullHeader, fullRows)
			cleanedCSV, err := encodeCSV(cleanHeader, cleanRows)
			if err != nil {
				j.fail(err)
				return
			}
			j.finish(enrichedCSV, cleanedCSV)
		}()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"job_id": id})
	}
}

func handleJobStatus(store *jobStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		j, ok := store.get(r.PathValue("id"))
		if !ok {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}

		status, processed, total, errMsg := j.snapshot()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":    status,
			"processed": processed,
			"total":     total,
			"error":     errMsg,
		})
	}
}

// handleJobResult returns the finished CSV. ?file=cleaned returns the
// reps' call-sheet variant instead of the full enriched output.
func handleJobResult(store *jobStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		j, ok := store.get(r.PathValue("id"))
		if !ok {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}

		status, _, _, errMsg := j.snapshot()
		if status != jobDone {
			if status == jobFailed {
				http.Error(w, fmt.Sprintf("job failed: %s", errMsg), http.StatusUnprocessableEntity)
				return
			}
			http.Error(w, fmt.Sprintf("job status is %q, not done", status), http.StatusConflict)
			return
		}

		j.mu.Lock()
		body := j.enrichedCSV
		if r.URL.Query().Get("file") == "cleaned" {
			body = j.cleanedCSV
		}
		j.mu.Unlock()

		w.Header().Set("Content-Type", "text/csv")
		w.Write(body)
	}
}

// handleEnrichOne runs the same enrichRow lookup used for bulk CSV rows
// against a single address and returns the result as a flat column-name ->
// value JSON object — the same shape as a row of the CSV output (same
// outputColumns keys, plus the address under the CSV's own input column
// names) — for on-demand single-target pulls (e.g. a rep pulling contacts
// for one target from the app UI) where the job-queue/polling flow the bulk
// CSV path uses would be pointless latency for one address. Returning the
// same shape as the CSV means the base44 side can reuse its existing
// CSV-row -> Target/Contact mapping instead of a second, divergent one.
func handleEnrichOne(c *clients) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var addr Address
		if err := json.NewDecoder(r.Body).Decode(&addr); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON body: %v", err), http.StatusBadRequest)
			return
		}
		if addr.Street == "" {
			http.Error(w, "street is required", http.StatusBadRequest)
			return
		}

		enr := enrichRow(r.Context(), c, addr)
		values := enr.toRow()

		row := map[string]string{
			"Address":      addr.Street,
			"Municipality": addr.City,
			"State":        addr.State,
			"ZIP Code":     addr.Zip,
		}
		for i, col := range outputColumns {
			if i < len(values) {
				row[col] = values[i]
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(row)
	}
}
