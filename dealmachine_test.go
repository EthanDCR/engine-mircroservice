package main

import (
	"context"
	"strings"
	"testing"
)

// Full-fidelity shape of the response that triggered the original bug:
// stories as a quoted string, with fields both before and after it.
const bodyStoriesString = `{"data":[{
  "matched": true,
  "address": "5100 State Highway 276",
  "city": "Royse City", "state": "TX", "zip": "75189",
  "year_built": 1998,
  "living_area_sqft": 2140,
  "roof_cover": "Asphalt Shingle",
  "property_type": "Single Family",
  "property_class": "Residential",
  "stories": "1 Story",
  "contacts": [{"full_name":"Jane Doe","contact_type":"owner","is_resident":true,
                "phones":[{"number":"5551234567","type":"mobile","do_not_call":false}]}]
}]}`

func TestParseRecoversEverythingAroundStories(t *testing.T) {
	res, err := parseDealMachineResult(context.Background(), []byte(bodyStoriesString))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Matched {
		t.Error("matched lost")
	}
	if res.Address != "5100 State Highway 276" {
		t.Errorf("resolved address lost: %q", res.Address)
	}
	if !res.YearBuilt.Valid || res.YearBuilt.Value != 1998 {
		t.Errorf("year_built lost: %+v", res.YearBuilt)
	}
	if !res.LivingAreaSqft.Valid || res.LivingAreaSqft.Value != 2140 {
		t.Errorf("living_area_sqft lost: %+v", res.LivingAreaSqft)
	}
	if res.RoofCover.String() != "Asphalt Shingle" {
		t.Errorf("roof_cover lost: %q", res.RoofCover.String())
	}
	if res.PropertyClass.String() != "Residential" {
		t.Errorf("property_class lost: %q", res.PropertyClass.String())
	}
	// contacts appear AFTER stories in the body — the field most at risk.
	if len(res.Contacts) != 1 || res.Contacts[0].FullName != "Jane Doe" {
		t.Errorf("contacts lost: %+v", res.Contacts)
	}
	if res.Stories.String() != "1 Story" {
		t.Errorf("stories not parsed: %q", res.Stories.String())
	}
}

// A type mismatch flexFloat can't absorb (an object where a number belongs)
// must still yield the rest of the property rather than a zero value.
func TestParseSurvivesUnabsorbableMismatch(t *testing.T) {
	body := `{"data":[{"matched":true,"address":"1 Main St","year_built":{"oops":1},
	           "contacts":[{"full_name":"Jane Doe"}]}]}`
	res, err := parseDealMachineResult(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Address != "1 Main St" || len(res.Contacts) != 1 {
		t.Errorf("partial result not preserved: %+v", res)
	}
	if res.YearBuilt.Valid {
		t.Errorf("offending field should stay unset, got %v", res.YearBuilt.Value)
	}
}

// Malformed JSON is a genuine failure and must still be reported as one.
func TestParseStillFailsOnMalformedJSON(t *testing.T) {
	if _, err := parseDealMachineResult(context.Background(), []byte(`{"data":[`)); err == nil {
		t.Fatal("expected error on truncated JSON, got nil")
	}
	if _, err := parseDealMachineResult(context.Background(), []byte(`not json at all`)); err == nil {
		t.Fatal("expected error on non-JSON body, got nil")
	}
}

func TestParseStillFailsOnEmptyData(t *testing.T) {
	_, err := parseDealMachineResult(context.Background(), []byte(`{"data":[]}`))
	if err == nil || !strings.Contains(err.Error(), "empty data array") {
		t.Fatalf("expected empty-data error, got %v", err)
	}
}

// A garbage shape in one field must not abort decoding and cost us the fields
// that follow it in the body — the failure mode a custom unmarshaler returning
// an error would reintroduce.
func TestFlexTypesNeverAbortDecoding(t *testing.T) {
	body := `{"data":[{"matched":true,"stories":{"weird":1},"roof_cover":42,
	           "owner_occupied":"not-a-bool","year_built":[1,2],
	           "address":"9 Last St","contacts":[{"full_name":"Jane Doe"}]}]}`
	res, err := parseDealMachineResult(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Address != "9 Last St" {
		t.Errorf("field after the garbage was dropped: %q", res.Address)
	}
	if len(res.Contacts) != 1 {
		t.Errorf("contacts after the garbage were dropped: %+v", res.Contacts)
	}
	if len(res.Stories) != 0 || res.YearBuilt.Valid || res.OwnerOccupied.Valid {
		t.Errorf("garbage should leave fields unset: %+v", res)
	}
	if len(res.RoofCover) != 0 {
		t.Errorf("garbage roof_cover should stay empty: %v", res.RoofCover)
	}
}

// Quoted scalars are accepted for every flex type, since stories proved
// DealMachine quotes inconsistently.
func TestFlexTypesAcceptQuotedScalars(t *testing.T) {
	body := `{"data":[{"matched":true,"year_built":"1998","living_area_sqft":"2140",
	           "owner_occupied":"true","stories":"2 Stories"}]}`
	res, err := parseDealMachineResult(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.YearBuilt.Valid || res.YearBuilt.Value != 1998 {
		t.Errorf("quoted year_built: %+v", res.YearBuilt)
	}
	if !res.LivingAreaSqft.Valid || res.LivingAreaSqft.Value != 2140 {
		t.Errorf("quoted living_area_sqft: %+v", res.LivingAreaSqft)
	}
	if !res.OwnerOccupied.Valid || !res.OwnerOccupied.Value {
		t.Errorf("quoted owner_occupied: %+v", res.OwnerOccupied)
	}
	if res.Stories.String() != "2 Stories" {
		t.Errorf("stories label: %q", res.Stories.String())
	}
}
