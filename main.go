package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// For a one-off local run, the CLI flow is:
//go run . -in newfile.csv -out newfile-enriched.csv
//python3 gen_callsheet.py newfile-enriched-cleaned.csv newfile-callsheet.html
//
// To run as a long-lived HTTP service instead:
//go run . serve -addr :8080

// requiredColumns maps the logical address parts this script needs to the
// column names present in the Kansas City parcel-export CSV. Adjust these
// if you point the script at a CSV with different header names.

var requiredColumns = map[string]string{
	"street": "Address",
	"city":   "Municipality",
	"state":  "State",
	"zip":    "ZIP Code",
	"owner":  "Owner",
}

// dropSourceColumns lists source CSV columns that are dropped from the
// output — administrative/land-use/legal-record fields not useful for
// door-knocking outreach. Everything else in the source CSV passes
// through untouched, in its original order.
var dropSourceColumns = map[string]bool{
	"Note":                    true,
	"Acreage":                 true,
	"Land Use Class":          true,
	"Land Use Code":           true,
	"Land Cover":              true,
	"Crop Cover":              true,
	"Elevation(Ft)":           true,
	"Legal Description 1":     true,
	"School District":         true,
	"Alternate ID 1":          true,
	"Alternate ID 2":          true,
	"Updated":                 true,
	"Robust Id":               true,
	"Market Value (Total)":    true,
	"Market Value (Land)":     true,
	"Market Value (Building)": true,
}

// filterSourceColumns returns the indices of header columns to keep (in
// original order) and the corresponding filtered header.
func filterSourceColumns(header []string) (keepIdx []int, filtered []string) {
	for i, name := range header {
		if dropSourceColumns[name] {
			continue
		}
		keepIdx = append(keepIdx, i)
		filtered = append(filtered, name)
	}
	return keepIdx, filtered
}

// repCleanDropExact lists exact column names cut from the reps' call-sheet
// CSV — administrative/geo/debug fields not useful when actually dialing.
var repCleanDropExact = map[string]bool{
	"County":                 true,
	"Parcel ID":              true,
	"Latitude":               true,
	"Longitude":              true,
	"Created":                true,
	"Tags":                   true,
	"Place":                  true,
	"Mailing Address 1":      true,
	"Mailing Address 2":      true,
	"Mailing Address 3":      true,
	"USPS Residential":       true,
	"Num Buildings":          true,
	"Section-Township-Range": true,
	"dealmachine_matched":    true,
	"stormpull_events_found": true,
	"building_type":          true,
	"owner_is_business":      true,
}

// isRepCleanDrop reports whether a column should be cut from the reps'
// call-sheet CSV. Deceased/carrier/dob are dropped for every person, not
// just the one instance requested — there's no reason to keep e.g.
// person 2's dob while dropping person 1's.
func isRepCleanDrop(col string) bool {
	if repCleanDropExact[col] {
		return true
	}
	return strings.Contains(col, "_deceased") || strings.Contains(col, "_carrier") || strings.Contains(col, "_dob")
}

// cleanedPathFor derives the reps' call-sheet path from the full output
// path: foo.csv -> foo-cleaned.csv.
func cleanedPathFor(outPath string) string {
	ext := filepath.Ext(outPath)
	base := strings.TrimSuffix(outPath, ext)
	return base + "-cleaned" + ext
}

// cleanForReps filters header/rows down to the reps' call-sheet column set.
func cleanForReps(header []string, rows [][]string) (cleanHeader []string, cleanRows [][]string) {
	var keepIdx []int
	for i, name := range header {
		if isRepCleanDrop(name) {
			continue
		}
		keepIdx = append(keepIdx, i)
		cleanHeader = append(cleanHeader, name)
	}

	cleanRows = make([][]string, len(rows))
	for i, row := range rows {
		cr := make([]string, 0, len(keepIdx))
		for _, idx := range keepIdx {
			cr = append(cr, row[idx])
		}
		cleanRows[i] = cr
	}
	return cleanHeader, cleanRows
}

func writeCleanedCSV(outPath string, header []string, rows [][]string) error {
	cleanHeader, cleanRows := cleanForReps(header, rows)
	return writeCSV(cleanedPathFor(outPath), cleanHeader, cleanRows)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serveFlags := flag.NewFlagSet("serve", flag.ExitOnError)
		addr := serveFlags.String("addr", ":8080", "address to listen on")
		serveFlags.Parse(os.Args[2:])

		if err := loadEnvFile(".env"); err != nil {
			log.Fatalf("loading .env: %v", err)
		}
		clients, err := newClients()
		if err != nil {
			log.Fatal(err)
		}
		runServe(*addr, clients)
		return
	}

	inPath := flag.String("in", "", "path to input CSV")
	outPath := flag.String("out", "", "path to write enriched output CSV")
	workers := flag.Int("workers", 5, "number of rows to process concurrently")
	flag.Parse()

	if *inPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: go run . -in input.csv -out output.csv [-workers 5]\n   or: go run . serve [-addr :8080]")
		os.Exit(1)
	}

	if err := loadEnvFile(".env"); err != nil {
		log.Fatalf("loading .env: %v", err)
	}

	clients, err := newClients()
	if err != nil {
		log.Fatal(err)
	}

	header, rows, err := readCSV(*inPath)
	if err != nil {
		log.Fatalf("reading input CSV: %v", err)
	}

	fullHeader, results, err := enrichCSV(context.Background(), clients, header, rows, *workers, nil)
	if err != nil {
		log.Fatal(err)
	}

	if err := writeCSV(*outPath, fullHeader, results); err != nil {
		log.Fatalf("writing output CSV: %v", err)
	}
	if err := writeCleanedCSV(*outPath, fullHeader, results); err != nil {
		log.Fatalf("writing cleaned output CSV: %v", err)
	}

	log.Printf("done: wrote %d rows to %s and %s", len(results), *outPath, cleanedPathFor(*outPath))
}

// enrichCSV is the shared core used by both the one-off CLI flow and the
// HTTP server's async jobs: resolve required columns, fan out enrichRow
// across a worker pool, and assemble the full output header/rows.
// onProgress, if non-nil, is called after each row completes.
func enrichCSV(ctx context.Context, c *clients, header []string, rows [][]string, workers int, onProgress func(done, total int)) (fullHeader []string, fullRows [][]string, err error) {
	colIdx, err := resolveColumns(header)
	if err != nil {
		return nil, nil, err
	}

	keepIdx, filteredHeader := filterSourceColumns(header)
	results := make([][]string, len(rows))

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	for i, row := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, row []string) {
			defer wg.Done()
			defer func() { <-sem }()

			addr := Address{
				Street: row[colIdx["street"]],
				City:   row[colIdx["city"]],
				State:  row[colIdx["state"]],
				Zip:    row[colIdx["zip"]],
			}

			enr := enrichRow(ctx, c, addr)

			out := make([]string, 0, len(keepIdx)+len(outputColumns))
			for _, idx := range keepIdx {
				out = append(out, row[idx])
			}
			out = append(out, enr.toRow()...)
			results[i] = out

			mu.Lock()
			done++
			n := done
			mu.Unlock()

			if onProgress != nil {
				onProgress(n, len(rows))
			}
			if n%25 == 0 || n == len(rows) {
				log.Printf("processed %d/%d rows", n, len(rows))
			}
		}(i, row)
	}
	wg.Wait()

	fullHeader = append(append([]string{}, filteredHeader...), outputColumns...)
	return fullHeader, results, nil
}

type clients struct {
	dealMachine *dealMachineClient
	batchData   *batchDataClient
	stormPull   *stormPullClient
}

func newClients() (*clients, error) {
	get := func(name string) (string, error) {
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("missing required env var %s (check scripts/.env)", name)
		}
		return v, nil
	}

	dmKey, err := get("DEALMACHINE_API_KEY")
	if err != nil {
		return nil, err
	}
	bdKey, err := get("BATCHDATA_API_KEY")
	if err != nil {
		return nil, err
	}
	spKey, err := get("STORMPULL_API_KEY")
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	return &clients{
		dealMachine: &dealMachineClient{apiKey: dmKey, http: httpClient, limiter: newRateLimiter(400 * time.Millisecond)},
		batchData:   &batchDataClient{apiKey: bdKey, http: httpClient},
		stormPull:   &stormPullClient{apiKey: spKey, http: httpClient},
	}, nil
}

// enrichRow runs the three independent branches (DealMachine, StormPull,
// BatchData) concurrently.
func enrichRow(ctx context.Context, c *clients, addr Address) enrichment {
	var enr enrichment
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := c.dealMachine.lookup(ctx, addr)
		if err != nil {
			enr.DealMachineError = err.Error()
			return
		}
		enr.DealMachineMatched = strconv.FormatBool(res.Matched)
		if res.YearBuilt != nil {
			enr.DealMachineYearBuilt = strconv.Itoa(*res.YearBuilt)
		}
		if res.LivingAreaSqft != nil {
			enr.DealMachineLivingAreaSqft = strconv.Itoa(*res.LivingAreaSqft)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := c.stormPull.lookup(ctx, addr)
		if err != nil {
			enr.StormPullError = err.Error()
			return
		}
		enr.StormPullEventsFound = strconv.Itoa(res.Results.EventsFound)
		if res.Score != nil {
			enr.StormPullExposureScore = fmt.Sprintf("%s · %d", res.Score.Tier, res.Score.Value)
			if res.Score.Summary.LargestHailInches > 0 {
				enr.StormPullMaxHailSizeIn = strconv.FormatFloat(res.Score.Summary.LargestHailInches, 'f', 2, 64)
			}
			enr.StormPullMaxHailDate = res.Score.Summary.LargestHailDate
			enr.StormPullLastEventDate = res.Score.Summary.MostRecentEventDate
			if res.Score.Summary.MostRecentHailInches > 0 {
				enr.StormPullLastEventHailSizeIn = strconv.FormatFloat(res.Score.Summary.MostRecentHailInches, 'f', 2, 64)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := c.batchData.skipTrace(ctx, addr)
		if err != nil {
			enr.BatchDataError = err.Error()
			return
		}
		if len(res.Property.Owners) > 0 {
			enr.BatchDataPropertyOwnerName = res.Property.Owners[0].Name.Full
		}

		for p := 0; p < maxPersons && p < len(res.Persons); p++ {
			src := res.Persons[p]
			out := personOut{
				Name:      src.Name.Full,
				Litigator: strconv.FormatBool(src.Litigator),
				Deceased:  strconv.FormatBool(src.Deceased),
				DOB:       src.DOB,
			}
			for ph := 0; ph < maxPhonesPerPerson && ph < len(src.Phones); ph++ {
				phone := src.Phones[ph]
				out.Phones[ph] = phoneOut{
					Number:    phone.Number,
					Type:      phone.Type,
					Carrier:   phone.Carrier,
					Tested:    strconv.FormatBool(phone.Tested),
					Reachable: strconv.FormatBool(phone.Reachable),
					DNC:       strconv.FormatBool(phone.DNC),
				}
			}
			for em := 0; em < maxEmailsPerPerson && em < len(src.Emails); em++ {
				out.Emails[em] = src.Emails[em].Email
			}
			enr.BatchDataPersons[p] = out
		}
	}()

	wg.Wait()
	return enr
}

func resolveColumns(header []string) (map[string]int, error) {
	idx := make(map[string]int, len(header))
	for i, name := range header {
		idx[name] = i
	}

	resolved := make(map[string]int, len(requiredColumns))
	for key, colName := range requiredColumns {
		i, ok := idx[colName]
		if !ok {
			return nil, fmt.Errorf("input CSV missing expected column %q (needed for %s)", colName, key)
		}
		resolved[key] = i
	}
	return resolved, nil
}

func readCSV(path string) (header []string, rows [][]string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return readCSVFrom(f)
}

func readCSVFrom(r io.Reader) (header []string, rows [][]string, err error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1

	all, err := cr.ReadAll()
	if err != nil {
		return nil, nil, err
	}
	if len(all) == 0 {
		return nil, nil, fmt.Errorf("input CSV is empty")
	}
	return all[0], all[1:], nil
}

func writeCSVTo(w io.Writer, header []string, rows [][]string) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, row := range rows {
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func writeCSV(path string, header []string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeCSVTo(f, header, rows)
}

func encodeCSV(header []string, rows [][]string) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCSVTo(&buf, header, rows); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
