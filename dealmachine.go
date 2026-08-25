package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const dealMachineURL = "https://api.v2.dealmachine.com/v1/enrichment/address"

type dealMachineClient struct {
	apiKey  string
	http    *http.Client
	limiter *rateLimiter
}

type dealMachineRequest struct {
	Data            []dealMachineAddressInput `json:"data"`
	Fields          []string                  `json:"fields,omitempty"`
	ContactAudience string                    `json:"contact_audience,omitempty"`
}

type dealMachineAddressInput struct {
	FullAddress string `json:"full_address"`
}

type dealMachineResponse struct {
	Data []dealMachineResult `json:"data"`
}

type dealMachineResult struct {
	Matched        bool `json:"matched"`
	YearBuilt      *int `json:"year_built"`
	LivingAreaSqft *int `json:"living_area_sqft"`
	MatchFailure   *struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	} `json:"match_failure"`
}

// lookup enriches a single address with property fields we don't get
// from the source CSV: year built and interior building square footage.
// contact_audience is explicitly "none" — owner contact info comes from
// BatchData, not DealMachine, per project decision. Responses are cached
// on disk by address so re-shaping what we extract never re-bills the
// lookup.
func (c *dealMachineClient) lookup(ctx context.Context, addr Address) (dealMachineResult, error) {
	key := fmt.Sprintf("%s|%s|%s|%s", addr.Street, addr.City, addr.State, addr.Zip)

	data, err := cachedFetch("dealmachine", key, func() ([]byte, error) {
		return c.fetch(ctx, addr)
	})
	if err != nil {
		return dealMachineResult{}, err
	}

	var parsed dealMachineResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return dealMachineResult{}, err
	}
	if len(parsed.Data) == 0 {
		return dealMachineResult{}, fmt.Errorf("dealmachine: empty data array in response")
	}
	return parsed.Data[0], nil
}

// fetch performs the actual HTTP call, retrying on 429 (rate limited)
// with exponential backoff — a burst of concurrent requests reliably
// triggers 429s here, and DealMachine only bills for matched results, so
// retrying a 429 costs nothing extra.
func (c *dealMachineClient) fetch(ctx context.Context, addr Address) ([]byte, error) {
	body := dealMachineRequest{
		Data: []dealMachineAddressInput{
			{FullAddress: fmt.Sprintf("%s, %s, %s %s", addr.Street, addr.City, addr.State, addr.Zip)},
		},
		Fields:          []string{"year_built", "living_area_sqft"},
		ContactAudience: "none",
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	const maxAttempts = 5
	backoff := 500 * time.Millisecond

	for attempt := 1; ; attempt++ {
		c.limiter.wait()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, dealMachineURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			if attempt >= maxAttempts {
				return nil, fmt.Errorf("dealmachine: rate limited after %d attempts", attempt)
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			backoff *= 2
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("dealmachine: unexpected status %d", resp.StatusCode)
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		return data, err
	}
}
