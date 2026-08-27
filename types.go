package main

import "fmt"

// Address is the common shape every enrichment API needs, built from
// whatever columns the input CSV actually has.
type Address struct {
	Street string
	City   string
	State  string
	Zip    string
}

// BatchData can return multiple associated persons per property and
// multiple ranked phones/emails per person — these caps are generous
// enough to cover what's been observed in practice (up to 5 phones, 3
// emails for a single person) without unbounded column growth. Extra
// entries beyond the cap are dropped; fewer than the cap leaves blank
// columns.
const (
	maxPersons         = 3
	maxPhonesPerPerson = 5
	maxEmailsPerPerson = 3

	// DealMachine returns at most one contact per owner on a property, but
	// co-owned properties can have several; caps mirror the BatchData ones
	// above for the same reason (generous enough in practice, bounded so
	// column count stays fixed).
	maxDealMachineContacts         = 3
	maxDealMachinePhonesPerContact = 5
	maxDealMachineEmailsPerContact = 3
)

type phoneOut struct {
	Number, Type, Carrier, Tested, Reachable, DNC string
}

type personOut struct {
	Name                string
	Litigator, Deceased string
	DOB                 string
	Phones              [maxPhonesPerPerson]phoneOut
	Emails              [maxEmailsPerPerson]string
}

type dmPhoneOut struct {
	Number, Type, DNC string
}

type dmContactOut struct {
	Name          string
	IsLikelyOwner string
	// BatchDataPhoneMatchScore is "1" if at least one of this contact's
	// DealMachine phone numbers also appeared in BatchData's results for
	// the same address, "0" otherwise. Matching numbers are cross-source
	// corroboration signal, not new information, so they're dropped from
	// Phones rather than duplicated — the score is what's kept instead.
	BatchDataPhoneMatchScore string
	Phones                   [maxDealMachinePhonesPerContact]dmPhoneOut
	Emails                   [maxDealMachineEmailsPerContact]string
}

// enrichment holds every field this pipeline adds to a CSV row. Fields
// left unset are written as empty columns in the output (roof type/size,
// building/property type, business flag — no source covers these yet).
type enrichment struct {
	DealMachineMatched        string
	DealMachineYearBuilt      string
	DealMachineLivingAreaSqft string
	DealMachineContacts       [maxDealMachineContacts]dmContactOut
	DealMachineError          string

	StormPullEventsFound         string
	StormPullMaxHailSizeIn       string
	StormPullMaxHailDate         string
	StormPullLastEventDate       string
	StormPullLastEventHailSizeIn string
	StormPullExposureScore       string
	StormPullError               string

	// BatchDataPropertyOwnerName is BatchData's own record of who owns the
	// property (property.owners[]) — distinct from, and not guaranteed to
	// match, any of the Persons below. propertyOwner on an individual
	// person is frequently null in BatchData's responses, so the persons
	// array should be read as "associated contacts", not "confirmed
	// owners".
	BatchDataPropertyOwnerName string
	BatchDataPersons           [maxPersons]personOut
	BatchDataError             string

	// No source covers these yet — always blank. Kept as columns so the
	// output CSV shape is stable once a 5th source is added.
	BuildingType    string
	PropertyType    string
	RoofType        string
	RoofSizeSqft    string
	OwnerIsBusiness string
}

// outputColumns lists every enrichment column in the order toRow()
// appends them, generating the BatchData person/phone/email block to
// match the caps above.
var outputColumns = buildOutputColumns()

func buildOutputColumns() []string {
	cols := []string{
		"dealmachine_matched",
		"dealmachine_year_built",
		"dealmachine_living_area_sqft",
	}

	for c := 1; c <= maxDealMachineContacts; c++ {
		cols = append(cols,
			fmt.Sprintf("dealmachine_contact%d_name", c),
			fmt.Sprintf("dealmachine_contact%d_is_likely_owner", c),
			fmt.Sprintf("dealmachine_contact%d_batchdata_phone_match_score", c),
		)
		for ph := 1; ph <= maxDealMachinePhonesPerContact; ph++ {
			cols = append(cols,
				fmt.Sprintf("dealmachine_contact%d_phone%d_number", c, ph),
				fmt.Sprintf("dealmachine_contact%d_phone%d_type", c, ph),
				fmt.Sprintf("dealmachine_contact%d_phone%d_dnc", c, ph),
			)
		}
		for em := 1; em <= maxDealMachineEmailsPerContact; em++ {
			cols = append(cols, fmt.Sprintf("dealmachine_contact%d_email%d", c, em))
		}
	}

	cols = append(cols,
		"dealmachine_error",

		"stormpull_events_found",
		"stormpull_max_hail_size_in",
		"stormpull_max_hail_date",
		"stormpull_last_event_date",
		"stormpull_last_event_hail_size_in",
		"stormpull_exposure_score",
		"stormpull_error",

		"batchdata_property_owner_name",
	)

	for p := 1; p <= maxPersons; p++ {
		cols = append(cols,
			fmt.Sprintf("batchdata_person%d_name", p),
			fmt.Sprintf("batchdata_person%d_litigator", p),
			fmt.Sprintf("batchdata_person%d_deceased", p),
			fmt.Sprintf("batchdata_person%d_dob", p),
		)
		for ph := 1; ph <= maxPhonesPerPerson; ph++ {
			cols = append(cols,
				fmt.Sprintf("batchdata_person%d_phone%d_number", p, ph),
				fmt.Sprintf("batchdata_person%d_phone%d_type", p, ph),
				fmt.Sprintf("batchdata_person%d_phone%d_carrier", p, ph),
				fmt.Sprintf("batchdata_person%d_phone%d_tested", p, ph),
				fmt.Sprintf("batchdata_person%d_phone%d_reachable", p, ph),
				fmt.Sprintf("batchdata_person%d_phone%d_dnc", p, ph),
			)
		}
		for em := 1; em <= maxEmailsPerPerson; em++ {
			cols = append(cols, fmt.Sprintf("batchdata_person%d_email%d", p, em))
		}
	}

	cols = append(cols,
		"batchdata_error",

		"building_type",
		"property_type",
		"roof_type",
		"roof_size_sqft",
		"owner_is_business",
	)
	return cols
}

func (e enrichment) toRow() []string {
	row := []string{
		e.DealMachineMatched,
		e.DealMachineYearBuilt,
		e.DealMachineLivingAreaSqft,
	}

	for c := 0; c < maxDealMachineContacts; c++ {
		contact := e.DealMachineContacts[c]
		row = append(row, contact.Name, contact.IsLikelyOwner, contact.BatchDataPhoneMatchScore)
		for ph := 0; ph < maxDealMachinePhonesPerContact; ph++ {
			phone := contact.Phones[ph]
			row = append(row, phone.Number, phone.Type, phone.DNC)
		}
		for em := 0; em < maxDealMachineEmailsPerContact; em++ {
			row = append(row, contact.Emails[em])
		}
	}

	row = append(row,
		e.DealMachineError,

		e.StormPullEventsFound,
		e.StormPullMaxHailSizeIn,
		e.StormPullMaxHailDate,
		e.StormPullLastEventDate,
		e.StormPullLastEventHailSizeIn,
		e.StormPullExposureScore,
		e.StormPullError,

		e.BatchDataPropertyOwnerName,
	)

	for p := 0; p < maxPersons; p++ {
		person := e.BatchDataPersons[p]
		row = append(row, person.Name, person.Litigator, person.Deceased, person.DOB)
		for ph := 0; ph < maxPhonesPerPerson; ph++ {
			phone := person.Phones[ph]
			row = append(row, phone.Number, phone.Type, phone.Carrier, phone.Tested, phone.Reachable, phone.DNC)
		}
		for em := 0; em < maxEmailsPerPerson; em++ {
			row = append(row, person.Emails[em])
		}
	}

	row = append(row,
		e.BatchDataError,

		e.BuildingType,
		e.PropertyType,
		e.RoofType,
		e.RoofSizeSqft,
		e.OwnerIsBusiness,
	)
	return row
}
