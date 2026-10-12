package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
)

const stormPullAddressURL = "https://stormpull.com/api/v1/hail/address"
const stormPullCoordinateURL = "https://stormpull.com/api/v1/hail/coordinate"

// Only events at/above this size count as "recent" for outreach purposes —
// a sub-roof-damage hit (say 0.6") a week ago shouldn't bump a genuinely
// severe 2" storm from a month earlier out of the "most recent event" field
// reps see. Score.Summary.MostRecentEventDate/Inches ignores this (and
// years_back) entirely — see mostRecentQualifyingEvent below — so this only
// takes effect because we compute "most recent" ourselves from the
// (correctly filtered) results.events list instead of trusting the summary.
const stormPullMinHailSizeIn = 1.25

// How far back to request. StormPull's own default is 5 and its max is 10.
// This was pinned at 1 while the only consumer was the "Events (12mo)"
// figure; it's 5 now because the app shows a per-storm claim window (each
// storm's statute of limitations counting down by state), which needs the
// actual event list, not just a count. results.events comes back with the
// request, so widening the window costs nothing extra per lookup.
const stormPullYearsBack = 5

type stormPullClient struct {
	apiKey string
	http   *http.Client
}

type stormPullEvent struct {
	Date           string  `json:"date"`
	HailSizeInches float64 `json:"hail_size_inches"`
}

type stormPullResponse struct {
	Results struct {
		EventsFound int              `json:"events_found"`
		Events      []stormPullEvent `json:"events"`
	} `json:"results"`
	Score *stormPullScore `json:"score"`
}

// mostRecentQualifyingEvent returns the latest-dated event in events (which
// the caller must have already fetched with min_hail_size applied), breaking
// ties on the same date by taking the largest hail size that day. Returns
// ok=false if events is empty.
func mostRecentQualifyingEvent(events []stormPullEvent) (date string, sizeIn float64, ok bool) {
	for _, e := range events {
		if !ok || e.Date > date || (e.Date == date && e.HailSizeInches > sizeIn) {
			date, sizeIn, ok = e.Date, e.HailSizeInches, true
		}
	}
	return date, sizeIn, ok
}

// sortedStormPullEvents returns the events newest-first, deduped by day with
// the largest size reported that day kept. StormPull can return several rows
// for one storm; a claim-window list wants one entry per date.
func sortedStormPullEvents(events []stormPullEvent) []hailEventOut {
	byDate := make(map[string]float64, len(events))
	for _, e := range events {
		if e.Date == "" || e.HailSizeInches <= 0 {
			continue
		}
		if cur, ok := byDate[e.Date]; !ok || e.HailSizeInches > cur {
			byDate[e.Date] = e.HailSizeInches
		}
	}
	out := make([]hailEventOut, 0, len(byDate))
	for d, sz := range byDate {
		out = append(out, hailEventOut{Date: d, Size: sz})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out
}

// hailEventOut is the shape handed downstream — deliberately `size` rather
// than `hail_size_inches` to match the consumer's stored hail_history.
type hailEventOut struct {
	Date string  `json:"date"`
	Size float64 `json:"size"`
}

// stormPullScore.Summary figures are computed from StormPull's full event
// history regardless of the years_back/min_hail_size request params (per
// their docs), so LargestHailInches/Date reflect the true all-time worst
// hail event at this address, not just what's in the requested window.
type stormPullScore struct {
	Value   int    `json:"value"`
	Tier    string `json:"tier"`
	Summary struct {
		LargestHailInches    float64 `json:"largest_hail_inches"`
		LargestHailDate      string  `json:"largest_hail_date"`
		MostRecentEventDate  string  `json:"most_recent_event_date"`
		MostRecentHailInches float64 `json:"most_recent_hail_inches"`
	} `json:"summary"`
}

// lookup fetches hail history for a single address, scoped to the last
// stormPullYearsBack years. Auth is via X-API-Key header, per StormPull's
// docs (not Bearer — confirmed against the live docs page). Responses
// are cached on disk by address (or coordinates, see fetch) so re-shaping
// what we extract never re-bills the lookup.
func (c *stormPullClient) lookup(ctx context.Context, addr Address) (stormPullResponse, error) {
	// min_hail_size and years_back change what StormPull returns, not just
	// what we parse out of it — they must both be part of the key, or
	// bumping either would keep serving responses fetched under the old
	// settings indefinitely.
	key := fmt.Sprintf("%s|%s|%s|%s", addr.Street, addr.City, addr.State, addr.Zip)
	if addr.Lat != nil && addr.Lng != nil {
		key = fmt.Sprintf("%s|coord|%.6f|%.6f", key, *addr.Lat, *addr.Lng)
	}
	key = fmt.Sprintf("%s|min%.2f|yb%d", key, stormPullMinHailSizeIn, stormPullYearsBack)

	data, err := cachedFetch(ctx, "stormpull", key, func() ([]byte, error) {
		return c.fetch(ctx, addr)
	})
	if err != nil {
		return stormPullResponse{}, err
	}

	var parsed stormPullResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return stormPullResponse{}, err
	}
	return parsed, nil
}

// fetch prefers /hail/coordinate over /hail/address whenever we already
// have lat/lng (e.g. a target already geocoded for the map) — /hail/address's
// geocoding step started rejecting valid addresses outright (confirmed
// against StormPull's own docs example and a canonical address), while
// /hail/coordinate returns real data for the exact same account/plan state.
// Both endpoints return the same `score` shape; only `results` differs
// slightly. min_hail_size/years_back only filter `results.events` (and
// `results.events_found`) — score.summary is computed from StormPull's full
// history regardless of these params (per their docs), which is why "most
// recent event" is derived from results.events ourselves rather than from
// score.summary.most_recent_event_date — see mostRecentQualifyingEvent.
func (c *stormPullClient) fetch(ctx context.Context, addr Address) ([]byte, error) {
	minHailSize := strconv.FormatFloat(stormPullMinHailSizeIn, 'f', -1, 64)
	yearsBack := strconv.Itoa(stormPullYearsBack)
	var u string
	if addr.Lat != nil && addr.Lng != nil {
		u = stormPullCoordinateURL + "?" + url.Values{
			"lat":           {strconv.FormatFloat(*addr.Lat, 'f', -1, 64)},
			"lon":           {strconv.FormatFloat(*addr.Lng, 'f', -1, 64)},
			"years_back":    {yearsBack},
			"min_hail_size": {minHailSize},
			"include_score": {"true"},
		}.Encode()
	} else {
		full := fmt.Sprintf("%s, %s, %s %s", addr.Street, addr.City, addr.State, addr.Zip)
		u = stormPullAddressURL + "?" + url.Values{
			"address":       {full},
			"years_back":    {yearsBack},
			"min_hail_size": {minHailSize},
			"include_score": {"true"},
		}.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		slog.WarnContext(ctx, "stormpull unexpected status", "req_id", reqID(ctx),
			"status", resp.StatusCode, "url", u, "body", string(body))
		return nil, fmt.Errorf("stormpull: unexpected status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
