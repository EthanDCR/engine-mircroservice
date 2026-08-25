package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const batchDataURL = "https://api.batchdata.com/api/v3/property/skip-trace"

type batchDataClient struct {
	apiKey string
	http   *http.Client
}

type batchDataRequest struct {
	Requests []batchDataRequestItem `json:"requests"`
}

type batchDataRequestItem struct {
	PropertyAddress batchDataAddress `json:"propertyAddress"`
}

type batchDataAddress struct {
	Street string `json:"street"`
	City   string `json:"city"`
	State  string `json:"state"`
	Zip    string `json:"zip"`
}

type batchDataResponse struct {
	Result struct {
		Data []batchDataResultItem `json:"data"`
	} `json:"result"`
}

type batchDataResultItem struct {
	Property struct {
		Owners []struct {
			Name struct {
				Full string `json:"full"`
			} `json:"name"`
		} `json:"owners"`
	} `json:"property"`
	Persons []batchDataPerson `json:"persons"`
	Meta    struct {
		Matched bool `json:"matched"`
		Error   bool `json:"error"`
	} `json:"meta"`
}

// batchDataPerson mirrors one entry in BatchData's `persons` array. Note
// PropertyOwner is frequently null (not true/false) in practice — BatchData
// doesn't always confidently identify which returned person is the actual
// owner, so callers should not assume persons[0] is "the owner". The
// authoritative owner name, when BatchData has one, is on
// batchDataResultItem.Property.Owners instead.
type batchDataPerson struct {
	PropertyOwner *bool `json:"propertyOwner"`
	Name          struct {
		Full string `json:"full"`
	} `json:"name"`
	Phones    []batchDataPhone `json:"phones"`
	Emails    []batchDataEmail `json:"emails"`
	Litigator bool             `json:"litigator"`
	Deceased  bool             `json:"deceased"`
	DOB       string           `json:"dob"`
}

type batchDataPhone struct {
	Rank      int    `json:"rank"`
	Number    string `json:"number"`
	Type      string `json:"type"`
	Carrier   string `json:"carrier"`
	Tested    bool   `json:"tested"`
	Reachable bool   `json:"reachable"`
	DNC       bool   `json:"dnc"`
}

type batchDataEmail struct {
	Rank  int    `json:"rank"`
	Email string `json:"email"`
}

// skipTrace looks up every associated person BatchData has for a property
// address (up to 3, per its docs), each with their full ranked phone and
// email lists — not just a single "best" contact. Responses are cached on
// disk by address so re-shaping what we extract from the response never
// re-bills the lookup.
func (c *batchDataClient) skipTrace(ctx context.Context, addr Address) (batchDataResultItem, error) {
	key := fmt.Sprintf("%s|%s|%s|%s", addr.Street, addr.City, addr.State, addr.Zip)

	data, err := cachedFetch("batchdata", key, func() ([]byte, error) {
		return c.fetch(ctx, addr)
	})
	if err != nil {
		return batchDataResultItem{}, err
	}

	var parsed batchDataResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return batchDataResultItem{}, err
	}
	if len(parsed.Result.Data) == 0 {
		return batchDataResultItem{}, fmt.Errorf("batchdata: empty data array in response")
	}
	return parsed.Result.Data[0], nil
}

func (c *batchDataClient) fetch(ctx context.Context, addr Address) ([]byte, error) {
	body := batchDataRequest{
		Requests: []batchDataRequestItem{
			{PropertyAddress: batchDataAddress{
				Street: addr.Street,
				City:   addr.City,
				State:  addr.State,
				Zip:    addr.Zip,
			}},
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, batchDataURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("batchdata: unexpected status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
