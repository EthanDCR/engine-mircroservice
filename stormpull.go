package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
// 12 months (yearsBack=1) to match the "Events (12mo)" figure the
// project is targeting. Auth is via X-API-Key header, per StormPull's
// docs (not Bearer — confirmed against the live docs page). Responses
// are cached on disk by address (or coordinates, see fetch) so re-shaping
// what we extract never re-bills the lookup.
func (c *stormPullClient) lookup(ctx context.Context, addr Address) (stormPullResponse, error) {
	// min_hail_size (and years_back, fixed at 1 below) change what StormPull
	// returns, not just what we parse out of it — they must be part of the
	// key, or bumping stormPullMinHailSizeIn would keep serving responses
	// fetched under the old threshold indefinitely.
	key := fmt.Sprintf("%s|%s|%s|%s", addr.Street, addr.City, addr.State, addr.Zip)
	if addr.Lat != nil && addr.Lng != nil {
		key = fmt.Sprintf("%s|coord|%.6f|%.6f", key, *addr.Lat, *addr.Lng)
	}
	key = fmt.Sprintf("%s|min%.2f", key, stormPullMinHailSizeIn)

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
	var u string
	if addr.Lat != nil && addr.Lng != nil {
		u = stormPullCoordinateURL + "?" + url.Values{
			"lat":           {strconv.FormatFloat(*addr.Lat, 'f', -1, 64)},
			"lon":           {strconv.FormatFloat(*addr.Lng, 'f', -1, 64)},
			"years_back":    {"1"},
			"min_hail_size": {minHailSize},
			"include_score": {"true"},
		}.Encode()
	} else {
		full := fmt.Sprintf("%s, %s, %s %s", addr.Street, addr.City, addr.State, addr.Zip)
		u = stormPullAddressURL + "?" + url.Values{
			"address":       {full},
			"years_back":    {"1"},
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
