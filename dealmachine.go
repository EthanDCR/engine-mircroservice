package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const dealMachineURL = "https://api.v2.dealmachine.com/v1/enrichment/address"
const dealMachineReverseGeocodeURL = "https://api.v2.dealmachine.com/v1/enrichment/reverse-geocode"

// dealMachineContactAudience controls which contacts DealMachine returns
// alongside property fields. "owners" costs people credits (unlike "none"),
// but gives us owner name/phone/email directly from DealMachine so it's
// available even for addresses BatchData fails to skip-trace.
const dealMachineContactAudience = "owners"

// dealMachineFields is shared between the actual API request and the cache
// key — the fields requested change what DealMachine returns, not just what
// we parse out of it, so a change here must bust the cache (same reasoning
// as stormPullMinHailSizeIn in stormpull.go) or already-cached addresses
// would keep serving responses fetched under the old field set forever.
var dealMachineFields = []string{"year_built", "living_area_sqft", "roof_cover"}

// flexStringList unmarshals a JSON field that's sometimes a single string
// and sometimes an array of strings (DealMachine's multi-select fields, e.g.
// roof_cover) into one consistent []string.
type flexStringList []string

func (f *flexStringList) UnmarshalJSON(data []byte) error {
	var multi []string
	if err := json.Unmarshal(data, &multi); err == nil {
		*f = multi
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return err
	}
	if single != "" {
		*f = []string{single}
	}
	return nil
}

func (f flexStringList) String() string {
	return strings.Join(f, "; ")
}

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

// dealMachineCoordInput is the /reverse-geocode counterpart to
// dealMachineAddressInput — same request/response envelope, but matched by
// coordinate against DealMachine's own parcel data instead of an address
// string.
type dealMachineCoordInput struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type dealMachineReverseGeocodeRequest struct {
	Data            []dealMachineCoordInput `json:"data"`
	Fields          []string                `json:"fields,omitempty"`
	ContactAudience string                  `json:"contact_audience,omitempty"`
}

type dealMachineResponse struct {
	Data []dealMachineResult `json:"data"`
}

type dealMachineResult struct {
	Matched bool `json:"matched"`
	// FullAddress/Address/City/State/Zip are DealMachine's own resolved
	// address for the matched parcel — present on both /address and
	// /reverse-geocode responses. Only /reverse-geocode's version of these
	// actually matters to callers: that's DealMachine's authoritative
	// address for a coordinate, used to correct a possibly-wrong
	// reverse-geocoded address before handing it to BatchData.
	FullAddress    string               `json:"full_address"`
	Address        string               `json:"address"`
	City           string               `json:"city"`
	State          string               `json:"state"`
	Zip            string               `json:"zip"`
	YearBuilt      *int                 `json:"year_built"`
	LivingAreaSqft *int                 `json:"living_area_sqft"`
	// RoofCover is the roof's material (asphalt shingle, tile, metal, slate)
	// — DealMachine's docs list this field as multi-select, so it's unmarshaled
	// via flexStringList to accept either a single string or a string array,
	// then joined for the flat output row. Not to be confused with
	// DealMachine's separate `roof_type` field (roof *shape* — gable/hip/
	// flat/shed), which we don't currently request or store.
	RoofCover      flexStringList       `json:"roof_cover"`
	Contacts       []dealMachineContact `json:"contacts"`
	MatchFailure   *struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	} `json:"match_failure"`
}

// dealMachineContact mirrors one entry in DealMachine's `contacts` array,
// present when the request's contact_audience is anything but "none". With
// contact_audience "owners" every entry should be a property owner, but
// IsLikelyOwner is kept so callers don't have to just trust that.
type dealMachineContact struct {
	FullName      string `json:"full_name"`
	IsLikelyOwner bool   `json:"is_likely_owner"`
	Phones        []struct {
		Number    string `json:"number"`
		Type      string `json:"type"`
		DoNotCall bool   `json:"do_not_call"`
	} `json:"phones"`
	Emails []struct {
		Address string `json:"address"`
	} `json:"emails"`
}

// lookup enriches a single address with property fields we don't get from
// the source CSV (year built, interior building square footage) and, when
// DealMachine has them, owner contacts (name/phone/email) — a fallback
// source of owner contact data alongside BatchData, since the two don't
// always match the same address. Responses are cached on disk by address
// and contact audience so re-shaping what we extract never re-bills the
// lookup, and changing the audience can't silently return a stale response
// fetched under a different one.
func (c *dealMachineClient) lookup(ctx context.Context, addr Address) (dealMachineResult, error) {
	key := fmt.Sprintf("%s|%s|%s|%s|%s|%s", addr.Street, addr.City, addr.State, addr.Zip,
		dealMachineContactAudience, strings.Join(dealMachineFields, ","))

	data, err := cachedFetch(ctx, "dealmachine", key, func() ([]byte, error) {
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

// reverseGeocode resolves DealMachine's own recorded parcel address for a
// coordinate — used instead of lookup() whenever we only have lat/lng from a
// map click. The Google reverse-geocode that produced the click's address
// string can land on the wrong house number (an interpolated guess along
// the street rather than the real parcel's recorded address), which then
// fails to match anything in BatchData. Matching by coordinate instead finds
// the actual parcel DealMachine has on file at that point and returns
// *its* address — which is the address BatchData's own records can
// actually match against. Cached the same way as lookup(), just keyed by
// coordinate instead of address string.
func (c *dealMachineClient) reverseGeocode(ctx context.Context, lat, lng float64) (dealMachineResult, error) {
	key := fmt.Sprintf("coord|%.6f|%.6f|%s|%s", lat, lng,
		dealMachineContactAudience, strings.Join(dealMachineFields, ","))

	data, err := cachedFetch(ctx, "dealmachine", key, func() ([]byte, error) {
		return c.fetchReverseGeocode(ctx, lat, lng)
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

func (c *dealMachineClient) fetch(ctx context.Context, addr Address) ([]byte, error) {
	body := dealMachineRequest{
		Data: []dealMachineAddressInput{
			{FullAddress: fmt.Sprintf("%s, %s, %s %s", addr.Street, addr.City, addr.State, addr.Zip)},
		},
		Fields:          dealMachineFields,
		ContactAudience: dealMachineContactAudience,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.postWithRetry(ctx, dealMachineURL, payload)
}

func (c *dealMachineClient) fetchReverseGeocode(ctx context.Context, lat, lng float64) ([]byte, error) {
	body := dealMachineReverseGeocodeRequest{
		Data:            []dealMachineCoordInput{{Latitude: lat, Longitude: lng}},
		Fields:          dealMachineFields,
		ContactAudience: dealMachineContactAudience,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.postWithRetry(ctx, dealMachineReverseGeocodeURL, payload)
}

// postWithRetry performs the actual HTTP call to a DealMachine enrichment
// endpoint (address or reverse-geocode — same auth/rate-limit/retry rules
// either way), retrying on 429 (rate limited) with exponential backoff — a
// burst of concurrent requests reliably triggers 429s here, and DealMachine
// only bills for matched results, so retrying a 429 costs nothing extra.
func (c *dealMachineClient) postWithRetry(ctx context.Context, url string, payload []byte) ([]byte, error) {
	const maxAttempts = 5
	backoff := 500 * time.Millisecond

	for attempt := 1; ; attempt++ {
		c.limiter.wait()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
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
			slog.WarnContext(ctx, "dealmachine rate limited", "req_id", reqID(ctx),
				"attempt", attempt, "backoff_ms", backoff.Milliseconds())
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
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			slog.WarnContext(ctx, "dealmachine unexpected status", "req_id", reqID(ctx),
				"status", resp.StatusCode, "body", string(body))
			return nil, fmt.Errorf("dealmachine: unexpected status %d", resp.StatusCode)
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		return data, err
	}
}
