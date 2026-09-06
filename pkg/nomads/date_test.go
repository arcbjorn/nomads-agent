package nomads

import (
	"encoding/json"
	"testing"
	"time"
)

// The central reason Date exists: a calendar date must not shift when it passes
// through timezones.
func TestDateSurvivesTimezoneRoundTrip(t *testing.T) {
	// Kiritimati is UTC+14; Niue is UTC-11. A naive UTC-midnight timestamp
	// rendered in either zone lands on a different calendar day.
	for _, zone := range []string{"Pacific/Kiritimati", "Pacific/Niue", "UTC", "America/Sao_Paulo"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Skipf("tzdata unavailable: %v", err)
		}
		d := MustParseDate("2026-09-25")
		// Whatever the local zone, the wire form is unchanged.
		if got := d.String(); got != "2026-09-25" {
			t.Errorf("%s: date drifted to %s", zone, got)
		}
		// And a wall-clock instant in that zone folds back to the same date.
		local := time.Date(2026, 9, 25, 23, 59, 0, 0, loc)
		if got := DateFromTime(local); got != d {
			t.Errorf("%s: DateFromTime gave %s, want %s", zone, got, d)
		}
	}
}

func TestDateJSONRoundTrip(t *testing.T) {
	type payload struct {
		From Date `json:"from"`
	}
	var p payload
	if err := json.Unmarshal([]byte(`{"from":"2026-10-04"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.From.String() != "2026-10-04" {
		t.Fatalf("got %s", p.From)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"from":"2026-10-04"}` {
		t.Fatalf("got %s", b)
	}
}

func TestDateJSONAcceptsEmptyAndNull(t *testing.T) {
	for _, in := range []string{`""`, `null`} {
		var d Date
		if err := json.Unmarshal([]byte(in), &d); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if !d.IsZero() {
			t.Fatalf("%s should decode to the zero date", in)
		}
	}
}

func TestParseDateRejectsGarbage(t *testing.T) {
	for _, in := range []string{"25-09-2026", "2026/09/25", "Sep 25 2026", "", "2026-13-01"} {
		if _, err := ParseDate(in); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestDateCompareAndOrdering(t *testing.T) {
	a, b := MustParseDate("2026-09-25"), MustParseDate("2026-10-04")
	if !a.Before(b) || b.Before(a) || !b.After(a) {
		t.Fatal("ordering is wrong")
	}
	if a.Compare(a) != 0 {
		t.Fatal("a date must equal itself")
	}
	// Ordering must be by calendar, not lexicographic accident.
	if !MustParseDate("2026-09-09").Before(MustParseDate("2026-09-10")) {
		t.Fatal("day ordering wrong")
	}
	if !MustParseDate("2026-02-28").Before(MustParseDate("2026-03-01")) {
		t.Fatal("month boundary ordering wrong")
	}
}

func TestDateArithmeticAcrossBoundaries(t *testing.T) {
	// Month boundary, the exact shape of the Sep 30 -> Oct 4 leg.
	if got := MustParseDate("2026-09-30").AddDays(4); got.String() != "2026-10-04" {
		t.Errorf("got %s", got)
	}
	// Leap day.
	if got := MustParseDate("2028-02-28").AddDays(1); got.String() != "2028-02-29" {
		t.Errorf("leap year: got %s", got)
	}
	// Non-leap year.
	if got := MustParseDate("2026-02-28").AddDays(1); got.String() != "2026-03-01" {
		t.Errorf("non-leap year: got %s", got)
	}
	// Year boundary, backwards.
	if got := MustParseDate("2027-01-01").AddDays(-1); got.String() != "2026-12-31" {
		t.Errorf("got %s", got)
	}
	if got := MustParseDate("2026-09-25").DaysUntil(MustParseDate("2026-09-30")); got != 5 {
		t.Errorf("want 5 days, got %d", got)
	}
}

func TestDateSQLRoundTrip(t *testing.T) {
	d := MustParseDate("2026-10-22")
	v, err := d.Value()
	if err != nil {
		t.Fatal(err)
	}
	var back Date
	if err := back.Scan(v); err != nil {
		t.Fatal(err)
	}
	if back != d {
		t.Fatalf("got %s want %s", back, d)
	}
	var zero Date
	if err := zero.Scan(nil); err != nil || !zero.IsZero() {
		t.Fatal("nil should scan to the zero date")
	}
}

func TestCanonicalNameFolding(t *testing.T) {
	cases := [][2]string{
		{"Kraków", "krakow"},
		{"  KRA   KOW  ", "kra kow"},
		{"Zürich", "zurich"},
		{"Málaga", "malaga"},
		{"İstanbul", "istanbul"},
		{"Tel-Aviv", "tel aviv"},
		{"Reykjavík", "reykjavik"},
	}
	for _, c := range cases {
		if got := CanonicalName(c[0]); got != c[1] {
			t.Errorf("CanonicalName(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	// Distinct cities must not collide.
	if CanonicalName("Vienna") == CanonicalName("Venice") {
		t.Error("distinct city names collided")
	}
}

func TestNormalizeTagsDedupesAndTrims(t *testing.T) {
	got := NormalizeTags([]string{"Web Dev", " Web Dev ", "web dev", "", "  ", "Sports"})
	if len(got) != 2 || got[0] != "Web Dev" || got[1] != "Sports" {
		t.Fatalf("got %#v", got)
	}
}

func TestTripStatusOn(t *testing.T) {
	tr := Trip{StartDate: MustParseDate("2026-09-25"), EndDate: MustParseDate("2026-09-30")}
	cases := []struct {
		day  string
		want TripStatus
	}{
		{"2026-09-01", TripPlanned},
		{"2026-09-25", TripCurrent}, // inclusive start
		{"2026-09-27", TripCurrent},
		{"2026-09-30", TripCurrent}, // inclusive end
		{"2026-10-01", TripCompleted},
	}
	for _, c := range cases {
		if got := tr.StatusOn(MustParseDate(c.day)); got != c.want {
			t.Errorf("on %s: got %s want %s", c.day, got, c.want)
		}
	}
}
