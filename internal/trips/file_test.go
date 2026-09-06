package trips

import (
	"testing"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

func TestParseYAMLDocumentedShape(t *testing.T) {
	got, err := ParseYAML(`
trips:
  - city: Lisbon
    country: Portugal
    from: 2030-04-10
    to: 2030-04-18

  - city: Porto
    country: Portugal
    from: 2030-04-18
    to: 2030-05-01

  - city: Malaga
    country: Spain
    from: 2030-05-01
    to: 2030-05-20
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 trips, got %d", len(got))
	}
	if got[0].City != "Lisbon" || got[0].Country != "Portugal" {
		t.Errorf("first trip: %+v", got[0])
	}
	if got[0].StartDate.String() != "2030-04-10" || got[0].EndDate.String() != "2030-04-18" {
		t.Errorf("dates: %s -> %s", got[0].StartDate, got[0].EndDate)
	}
	if got[2].City != "Malaga" || got[2].EndDate.String() != "2030-05-20" {
		t.Errorf("last trip: %+v", got[2])
	}
}

func TestParseYAMLHandlesQuotesCommentsAndAliases(t *testing.T) {
	got, err := ParseYAML(`
# a leading comment
trips:
  - city: "San José"      # inline comment
    country: 'Costa Rica'
    start: 2026-01-01
    end: 2026-01-10
    note: "beach # not a comment"
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 trip, got %d", len(got))
	}
	if got[0].City != "San José" || got[0].Country != "Costa Rica" {
		t.Errorf("quotes not stripped: %+v", got[0])
	}
	if got[0].Note != "beach # not a comment" {
		t.Errorf("comment stripped inside quotes: %q", got[0].Note)
	}
	if got[0].StartDate.String() != "2026-01-01" {
		t.Errorf("start/end aliases not accepted: %s", got[0].StartDate)
	}
}

func TestParseYAMLRejectsUnknownFieldsAndBadDates(t *testing.T) {
	if _, err := ParseYAML("trips:\n  - city: Rome\n    colour: red\n"); err == nil {
		t.Error("expected an error for an unknown field")
	}
	if _, err := ParseYAML("trips:\n  - city: Rome\n    from: 25-09-2026\n"); err == nil {
		t.Error("expected an error for a malformed date")
	}
	if _, err := ParseYAML("cities:\n  - city: Rome\n"); err == nil {
		t.Error("expected an error when there is no trips list")
	}
}

func TestParseJSONShape(t *testing.T) {
	got, err := parseJSON([]byte(`{"trips":[
		{"city":"Malaga","country":"Spain","from":"2030-05-01","to":"2030-05-20"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].City != "Malaga" {
		t.Fatalf("got %+v", got)
	}
	if got[0].StartDate.String() != "2030-05-01" {
		t.Errorf("date: %s", got[0].StartDate)
	}
}

func TestParsedTripsValidate(t *testing.T) {
	got, err := ParseYAML("trips:\n  - city: Rome\n    country: Portugal\n    from: 2026-04-01\n    to: 2026-04-10\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range got {
		if err := tr.Validate(); err != nil {
			t.Errorf("%s: %v", tr.City, err)
		}
	}
	_ = nomads.DesiredTrip{}
}
