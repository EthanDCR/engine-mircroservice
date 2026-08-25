package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const stormPullURL = "https://stormpull.com/api/v1/hail/address"

type stormPullClient struct {
	apiKey string
	http   *http.Client
}

type stormPullResponse struct {
	Results struct {
		EventsFound int `json:"events_found"`
	} `json:"results"`
	Score *stormPullScore `json:"score"`
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
// are cached on disk by address so re-shaping what we extract never
// re-bills the lookup.
func (c *stormPullClient) lookup(ctx context.Context, addr Address) (stormPullResponse, error) {
	key := fmt.Sprintf("%s|%s|%s|%s", addr.Street, addr.City, addr.State, addr.Zip)

	data, err := cachedFetch("stormpull", key, func() ([]byte, error) {
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

func (c *stormPullClient) fetch(ctx context.Context, addr Address) ([]byte, error) {
	full := fmt.Sprintf("%s, %s, %s %s", addr.Street, addr.City, addr.State, addr.Zip)
	u := stormPullURL + "?" + url.Values{
		"address":       {full},
		"years_back":    {"1"},
		"include_score": {"true"},
	}.Encode()

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
		return nil, fmt.Errorf("stormpull: unexpected status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
