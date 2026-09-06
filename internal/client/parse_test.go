package client

import (
	"os"
	"testing"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../fixtures/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

func TestParseTripsFromFixture(t *testing.T) {
	trips, err := parseTrips(loadFixture(t, "profile.html"))
	if err != nil {
		t.Fatal(err)
	}
	// Three real trips; the editor template row must be skipped.
	if len(trips) != 3 {
		t.Fatalf("want 3 trips, got %d: %+v", len(trips), trips)
	}

	// Sorted chronologically, so the 2023 trip comes first.
	if trips[0].City != "Málaga" {
		t.Errorf("trips not sorted chronologically: first is %q", trips[0].City)
	}
	if trips[0].StartDate.String() != "2029-02-03" || trips[0].EndDate.String() != "2029-02-11" {
		t.Errorf("bad dates: %s -> %s", trips[0].StartDate, trips[0].EndDate)
	}
	if trips[0].Country != "Spain" || trips[0].Slug != "malaga-spain" {
		t.Errorf("bad country/slug: %q %q", trips[0].Country, trips[0].Slug)
	}
	if trips[0].Latitude == 0 || trips[0].Longitude == 0 {
		t.Errorf("coordinates not parsed: %f %f", trips[0].Latitude, trips[0].Longitude)
	}

	lisbon := trips[1]
	if lisbon.City != "Lisbon" || lisbon.ID != "aaaa0000000000000000000000000000000000000000000001" {
		t.Errorf("unexpected trip: %+v", lisbon)
	}
	// The temperature widget inside the .name cell must not leak into the city.
	if lisbon.City != "Lisbon" {
		t.Errorf("city contaminated by sibling markup: %q", lisbon.City)
	}
}

func TestParseTripsDecodesHTMLEntities(t *testing.T) {
	trips, err := parseTrips(loadFixture(t, "profile.html"))
	if err != nil {
		t.Fatal(err)
	}
	// "S&atilde;o Paulo" must come back as real text so matching works.
	if trips[0].City != "Málaga" {
		t.Fatalf("entity not decoded: %q", trips[0].City)
	}
	if nomads.CanonicalName(trips[0].City) != "malaga" {
		t.Fatalf("canonicalisation failed: %q", nomads.CanonicalName(trips[0].City))
	}
}

func TestParseProfileFromFixture(t *testing.T) {
	p := parseProfile(loadFixture(t, "profile.html"))

	if p.Bio != "Remote developer, currently in Europe." {
		t.Errorf("bio: %q", p.Bio)
	}
	if p.Website != "https://example.com/" {
		t.Errorf("website: %q", p.Website)
	}
	if p.Twitter != "exampleuser" || p.Instagram != "exampleuser" {
		t.Errorf("socials: %q %q", p.Twitter, p.Instagram)
	}
	// Empty inputs must be empty strings, not the literal markup.
	if p.YouTube != "" || p.TikTok != "" {
		t.Errorf("expected empty youtube/tiktok, got %q %q", p.YouTube, p.TikTok)
	}
}

func TestParseTagsOnlyReturnsActive(t *testing.T) {
	tags := parseTags(loadFixture(t, "profile.html"))

	if len(tags) != 3 {
		t.Fatalf("want 3 active tags, got %d: %v", len(tags), tags)
	}
	want := map[string]bool{"Web Dev": true, "Software Dev": true, "Sports": true}
	for _, tag := range tags {
		if !want[tag] {
			t.Errorf("unexpected tag %q (inactive tags must be excluded)", tag)
		}
	}
}

// A page whose trip rows lost their data attributes should be reported as an
// API change, not silently parsed as "no trips" — which would make a sync plan
// try to recreate the user's entire history.
func TestParseTripsDetectsMarkupChange(t *testing.T) {
	broken := `<table><tr class="trip"><td class="name"><h2>Lisbon</h2></td></tr>
	           <tr class="trip"><td class="name"><h2>Porto</h2></td></tr></table>`
	_, err := parseTrips(broken)
	if err == nil {
		t.Fatal("expected an error when trip rows cannot be parsed")
	}
	if !nomads.IsCode(err, nomads.ErrAPIChanged) {
		t.Fatalf("want NOMADS_API_CHANGED, got %s", nomads.CodeOf(err))
	}
}

// A profile with genuinely no trips is legitimate and must not error.
func TestParseTripsEmptyProfileIsNotAnError(t *testing.T) {
	trips, err := parseTrips(`<html><body><table class="trips"></table></body></html>`)
	if err != nil {
		t.Fatalf("empty profile should not error: %v", err)
	}
	if len(trips) != 0 {
		t.Fatalf("want 0 trips, got %d", len(trips))
	}
}

func TestSlugCity(t *testing.T) {
	cases := [][2]string{
		{"lisbon-portugal", "Lisbon"},
		{"porto-portugal", "Porto"},
		{"malaga-spain", "Malaga"},
	}
	for _, c := range cases {
		if got := slugCity(c[0]); got != c[1] {
			t.Errorf("slugCity(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}
