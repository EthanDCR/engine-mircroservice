package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
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
	initLogging()

	if len(os.Args) > 1 && os.Args[1] == "serve" {
		// Render (and most PaaS platforms) assign the port via $PORT and
		// expect the app to bind to it — there's no picking your own.
		defaultAddr := ":8080"
		if port := os.Getenv("PORT"); port != "" {
			defaultAddr = ":" + port
		}

		serveFlags := flag.NewFlagSet("serve", flag.ExitOnError)
		addr := serveFlags.String("addr", defaultAddr, "address to listen on")
		serveFlags.Parse(os.Args[2:])

		if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
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

	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
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

	ctx := withReqID(context.Background(), newReqID())
	fullHeader, results, err := enrichCSV(ctx, clients, header, rows, *workers, nil)
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
	slog.InfoContext(ctx, "enrichCSV starting", "req_id", reqID(ctx), "rows", len(rows), "workers", workers)

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
	var dmErrs, bdErrs, spErrs int

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
			if enr.DealMachineError != "" {
				dmErrs++
			}
			if enr.BatchDataError != "" {
				bdErrs++
			}
			if enr.StormPullError != "" {
				spErrs++
			}
			mu.Unlock()

			if onProgress != nil {
				onProgress(n, len(rows))
			}
			if n%25 == 0 || n == len(rows) {
				slog.InfoContext(ctx, "enrichCSV progress", "req_id", reqID(ctx), "done", n, "total", len(rows))
			}
		}(i, row)
	}
	wg.Wait()

	slog.InfoContext(ctx, "enrichCSV done", "req_id", reqID(ctx), "rows", len(rows),
		"dealmachine_errors", dmErrs, "batchdata_errors", bdErrs, "stormpull_errors", spErrs)

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

// businessNameKeywords catches the common ways a property owner shows up as
// a business entity rather than a person on a deed — BatchData/DealMachine
// don't classify this themselves, they just hand back whatever name string
// is on file, LLC suffix and all.
var businessNameKeywords = []string{
	"LLC", "L.L.C", "INC", "INCORPORATED", "CORP", "CORPORATION", " LP", "L.P",
	"LTD", "TRUST", "LLP", "HOLDINGS", "PROPERTIES", "PARTNERS", "ENTERPRISES",
	"GROUP", "COMPANY", "ASSOCIATES", "REALTY", "INVESTMENTS", "CHURCH",
	"FOUNDATION", "MINISTRIES", "PORTFOLIO", "CAPITAL", "VENTURES", "FUND",
}

func isBusinessName(name string) bool {
	upper := " " + strings.ToUpper(name) + " "
	for _, kw := range businessNameKeywords {
		if strings.Contains(upper, strings.ToUpper(kw)) {
			return true
		}
	}
	return false
}

// resultLogAttrs summarizes one address + its enrichment result into log
// fields — the address/coordinates queried and a compact readout of what
// each provider actually returned (or its error), so a single log line
// answers "what did we look up and what came back" without dumping the
// full row.
func resultLogAttrs(addr Address, enr enrichment) []any {
	attrs := []any{
		"street", addr.Street, "city", addr.City, "state", addr.State, "zip", addr.Zip,
	}
	if addr.Lat != nil && addr.Lng != nil {
		attrs = append(attrs, "lat", *addr.Lat, "lng", *addr.Lng)
	}

	if enr.DealMachineError != "" {
		attrs = append(attrs, "dealmachine_error", enr.DealMachineError)
	} else {
		attrs = append(attrs, "dealmachine_matched", enr.DealMachineMatched,
			"dealmachine_year_built", enr.DealMachineYearBuilt,
			"dealmachine_owner_occupied", enr.DealMachineOwnerOccupied,
			"dealmachine_contacts", nonEmptyDealMachineContacts(enr),
			// Actual name(s), not just the count above, and whether each
			// looks like an LLC/business entity rather than a person — meant
			// to be eyeballed directly against what the app shows for the
			// same target, the same way batchdata_mailing_addresses is below.
			"dealmachine_contact_names", dealMachineContactNames(enr),
			// enr.RoofType is DealMachine's roof_cover (material), not its
			// separate roof_type (shape) field — see types.go.
			"dealmachine_roof_cover", enr.RoofType)
	}
	if enr.DealMachineResolvedStreet != "" {
		attrs = append(attrs, "dealmachine_resolved_address",
			fmt.Sprintf("%s, %s, %s %s", enr.DealMachineResolvedStreet, enr.DealMachineResolvedCity,
				enr.DealMachineResolvedState, enr.DealMachineResolvedZip))
	}

	if enr.BatchDataError != "" {
		attrs = append(attrs, "batchdata_error", enr.BatchDataError)
	} else {
		attrs = append(attrs, "batchdata_owner", enr.BatchDataPropertyOwnerName,
			// OwnerIsBusiness is computed from BatchDataPropertyOwnerName
			// (see isBusinessName) but was never actually logged anywhere —
			// there was no way to tell from the logs alone whether an LLC
			// got flagged as one. Note it's blank whenever BatchData didn't
			// return an owner name at all, even if DealMachine's contact
			// above is clearly a business — the two aren't cross-checked.
			"batchdata_owner_is_business", enr.OwnerIsBusiness,
			"batchdata_persons", nonEmptyBatchDataPersons(enr),
			// Actual values (name: mailing address), not just a count — this
			// is meant to be diffed directly against what the app shows for
			// the same target, not just confirm data existed.
			"batchdata_mailing_addresses", batchDataMailingAddresses(enr))
	}

	if enr.StormPullError != "" {
		attrs = append(attrs, "stormpull_error", enr.StormPullError)
	} else {
		attrs = append(attrs, "stormpull_events_found", enr.StormPullEventsFound,
			"stormpull_exposure_score", enr.StormPullExposureScore,
			"stormpull_max_hail_in", enr.StormPullMaxHailSizeIn,
			"stormpull_max_hail_date", enr.StormPullMaxHailDate)
	}

	return attrs
}

func nonEmptyDealMachineContacts(enr enrichment) int {
	n := 0
	for _, c := range enr.DealMachineContacts {
		if c.Name != "" {
			n++
		}
	}
	return n
}

// dealMachineContactNames renders "Name (owner, LLC)" for every DealMachine
// contact with a name — flags contact_type/is_resident and business-entity
// status the same way isBusinessName does for BatchData, so an LLC coming
// back from DealMachine is visible in the log even on a row where BatchData
// errored or returned no owner name at all (the two sources aren't merged).
func dealMachineContactNames(enr enrichment) string {
	var parts []string
	for _, c := range enr.DealMachineContacts {
		if c.Name == "" {
			continue
		}
		var tags []string
		if c.ContactType != "" {
			tags = append(tags, c.ContactType)
		}
		if c.IsResident == "true" {
			tags = append(tags, "resident")
		}
		if isBusinessName(c.Name) {
			tags = append(tags, "LLC")
		}
		if len(tags) == 0 {
			parts = append(parts, c.Name)
		} else {
			parts = append(parts, fmt.Sprintf("%s (%s)", c.Name, strings.Join(tags, ", ")))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " | ")
}

func nonEmptyBatchDataPersons(enr enrichment) int {
	n := 0
	for _, p := range enr.BatchDataPersons {
		if p.Name != "" {
			n++
		}
	}
	return n
}

// batchDataMailingAddresses renders "Name: address" for every person that
// has one, e.g. "John Doe: 123 Main St, Springfield IL 62704" — logged
// alongside the request so a rep-reported discrepancy (app shows nothing,
// or shows something different) can be checked directly against what the
// engine actually got back, not just whether *a* mailing address existed.
func batchDataMailingAddresses(enr enrichment) string {
	var parts []string
	for _, p := range enr.BatchDataPersons {
		if p.MailingAddress == "" {
			continue
		}
		name := p.Name
		if name == "" {
			name = "(unnamed)"
		}
		parts = append(parts, name+": "+p.MailingAddress)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " | ")
}

// enrichRow runs the three independent API lookups (DealMachine, StormPull,
// BatchData) concurrently, then reconciles DealMachine's contacts against
// BatchData's afterward — that step needs both results at once, so it can't
// happen inside either goroutine.
//
// When addr carries lat/lng (a map click, never a bulk CSV row), DealMachine
// is queried by coordinate instead of by address string, and BatchData's
// call waits on that result and uses DealMachine's resolved address instead
// of addr — see reverseGeocode in dealmachine.go for why. StormPull isn't
// affected either way: it already takes coordinates directly when present.
func enrichRow(ctx context.Context, c *clients, addr Address) enrichment {
	var enr enrichment
	var wg sync.WaitGroup

	var dmRes dealMachineResult
	var dmErr error
	var dmWG sync.WaitGroup
	dmWG.Add(1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer dmWG.Done()
		if addr.Lat != nil && addr.Lng != nil {
			dmRes, dmErr = c.dealMachine.reverseGeocode(ctx, *addr.Lat, *addr.Lng)
		} else {
			dmRes, dmErr = c.dealMachine.lookup(ctx, addr)
		}
	}()

	var bdRes batchDataResultItem
	var bdErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := c.stormPull.lookup(ctx, addr)
		if err != nil {
			slog.WarnContext(ctx, "stormpull lookup failed", "req_id", reqID(ctx), "street", addr.Street, "err", err)
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
		}
		// score.summary.most_recent_event_date/hail_inches ignores
		// min_hail_size (see fetch's doc comment) — an insignificant event a
		// few days ago would otherwise outrank a real storm from a month
		// earlier. Derive "most recent" ourselves from results.events, which
		// IS filtered to stormPullMinHailSizeIn+.
		if date, size, ok := mostRecentQualifyingEvent(res.Results.Events); ok {
			enr.StormPullLastEventDate = date
			enr.StormPullLastEventHailSizeIn = strconv.FormatFloat(size, 'f', 2, 64)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		bdAddr := addr
		if addr.Lat != nil && addr.Lng != nil {
			// Only known once DealMachine's coordinate match resolves.
			// Bulk CSV rows never carry lat/lng, so they never reach this
			// wait and DealMachine/BatchData stay fully parallel for them.
			dmWG.Wait()
			if dmErr == nil && dmRes.Matched && dmRes.Address != "" {
				bdAddr = Address{Street: dmRes.Address, City: dmRes.City, State: dmRes.State, Zip: dmRes.Zip}
			}
		}
		bdRes, bdErr = c.batchData.skipTrace(ctx, bdAddr)
	}()

	wg.Wait()

	if addr.Lat != nil && addr.Lng != nil && dmErr == nil && dmRes.Matched && dmRes.Address != "" {
		enr.DealMachineResolvedStreet = dmRes.Address
		enr.DealMachineResolvedCity = dmRes.City
		enr.DealMachineResolvedState = dmRes.State
		enr.DealMachineResolvedZip = dmRes.Zip
	}

	if bdErr != nil {
		slog.WarnContext(ctx, "batchdata lookup failed", "req_id", reqID(ctx), "street", addr.Street, "err", bdErr)
		enr.BatchDataError = bdErr.Error()
	} else {
		if len(bdRes.Property.Owners) > 0 {
			// A property can have more than one owner on title (e.g. an LLC
			// plus an individual, or joint owners) — join them all instead of
			// keeping only the first and silently dropping the rest.
			names := make([]string, 0, len(bdRes.Property.Owners))
			for _, o := range bdRes.Property.Owners {
				if o.Name.Full != "" {
					names = append(names, o.Name.Full)
				}
			}
			enr.BatchDataPropertyOwnerName = strings.Join(names, "; ")
			enr.OwnerIsBusiness = strconv.FormatBool(isBusinessName(enr.BatchDataPropertyOwnerName))
		}

		for p := 0; p < maxPersons && p < len(bdRes.Persons); p++ {
			src := bdRes.Persons[p]
			out := personOut{
				Name:           src.Name.Full,
				Litigator:      strconv.FormatBool(src.Litigator),
				Deceased:       strconv.FormatBool(src.Deceased),
				DOB:            src.DOB,
				MailingAddress: src.MailingAddress.String(),
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
	}

	// BatchData phone numbers already on this row, so DealMachine contacts
	// below can drop numbers we already have instead of listing them twice.
	batchDataPhones := make(map[string]bool)
	for _, person := range bdRes.Persons {
		for _, phone := range person.Phones {
			batchDataPhones[normalizePhoneDigits(phone.Number)] = true
		}
	}

	if dmErr != nil {
		slog.WarnContext(ctx, "dealmachine lookup failed", "req_id", reqID(ctx), "street", addr.Street, "err", dmErr)
		enr.DealMachineError = dmErr.Error()
	} else {
		enr.DealMachineMatched = strconv.FormatBool(dmRes.Matched)
		if dmRes.YearBuilt != nil {
			enr.DealMachineYearBuilt = strconv.Itoa(*dmRes.YearBuilt)
		}
		if dmRes.LivingAreaSqft != nil {
			enr.DealMachineLivingAreaSqft = strconv.Itoa(*dmRes.LivingAreaSqft)
		}
		if len(dmRes.RoofCover) > 0 {
			enr.RoofType = dmRes.RoofCover.String()
		}
		if dmRes.OwnerOccupied != nil {
			enr.DealMachineOwnerOccupied = strconv.FormatBool(*dmRes.OwnerOccupied)
		}

		for i := 0; i < maxDealMachineContacts && i < len(dmRes.Contacts); i++ {
			src := dmRes.Contacts[i]
			out := dmContactOut{
				Name:        src.FullName,
				ContactType: src.ContactType,
				IsResident:  strconv.FormatBool(src.IsResident),
			}
			matched := false
			slot := 0
			for _, phone := range src.Phones {
				if batchDataPhones[normalizePhoneDigits(phone.Number)] {
					matched = true
					continue // already have this number from BatchData; don't list it twice
				}
				if slot >= maxDealMachinePhonesPerContact {
					continue
				}
				out.Phones[slot] = dmPhoneOut{
					Number: phone.Number,
					Type:   phone.Type,
					DNC:    strconv.FormatBool(phone.DoNotCall),
				}
				slot++
			}
			if matched {
				out.BatchDataPhoneMatchScore = "1"
			} else {
				out.BatchDataPhoneMatchScore = "0"
			}
			for em := 0; em < maxDealMachineEmailsPerContact && em < len(src.Emails); em++ {
				out.Emails[em] = src.Emails[em].Address
			}
			enr.DealMachineContacts[i] = out
		}
	}

	// Debug (not Info) because a bulk CSV job runs this per row — thousands
	// of these at Info would drown out everything else in Render's log
	// view. The per-row progress log in enrichCSV covers the default case;
	// set LOG_LEVEL=debug to see every address + what came back.
	slog.DebugContext(ctx, "enrichRow result", append([]any{"req_id", reqID(ctx)}, resultLogAttrs(addr, enr)...)...)

	return enr
}

// normalizePhoneDigits strips a number down to its bare digits (dropping a
// leading US country code "1" on 11-digit numbers) so phone numbers from
// different providers — which format them differently — can be compared
// for an exact match.
func normalizePhoneDigits(num string) string {
	var b strings.Builder
	for _, r := range num {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if len(digits) == 11 && digits[0] == '1' {
		digits = digits[1:]
	}
	return digits
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
