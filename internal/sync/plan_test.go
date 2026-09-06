package sync

import (
	"strings"
	"testing"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

func d(s string) nomads.Date { return nomads.MustParseDate(s) }

func trip(id, city, country, from, to string) nomads.Trip {
	return nomads.Trip{ID: id, City: city, Country: country, StartDate: d(from), EndDate: d(to)}
}

func want(city, country, from, to string) nomads.DesiredTrip {
	return nomads.DesiredTrip{City: city, Country: country, StartDate: d(from), EndDate: d(to)}
}

// The headline use case: three new trips against an empty remote.
func TestPlanCreatesAllWhenRemoteEmpty(t *testing.T) {
	desired := []nomads.DesiredTrip{
		want("Lisbon", "Portugal", "2030-04-10", "2030-04-18"),
		want("Porto", "Portugal", "2030-04-18", "2030-05-01"),
		want("Malaga", "Spain", "2030-05-01", "2030-05-20"),
	}
	p := PlanTripSync(nil, desired, Options{})

	if len(p.Creates) != 3 {
		t.Fatalf("want 3 creates, got %d", len(p.Creates))
	}
	if p.MutationCount() != 3 || p.IsNoop() {
		t.Fatalf("expected 3 mutations, got %d (noop=%v)", p.MutationCount(), p.IsNoop())
	}
	// Creates must come out in chronological order.
	if p.Creates[0].City != "Lisbon" || p.Creates[2].City != "Malaga" {
		t.Fatalf("creates out of order: %s, %s, %s",
			p.Creates[0].City, p.Creates[1].City, p.Creates[2].City)
	}
	// Back-to-back trips sharing a travel day are not an overlap warning.
	if len(p.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", p.Warnings)
	}
}

// Running the same desired state against matching remote state must be a no-op.
func TestPlanIsIdempotent(t *testing.T) {
	desired := []nomads.DesiredTrip{
		want("Lisbon", "Portugal", "2030-04-10", "2030-04-18"),
		want("Malaga", "Spain", "2030-05-01", "2030-05-20"),
	}
	current := []nomads.Trip{
		trip("t1", "Lisbon", "Portugal", "2030-04-10", "2030-04-18"),
		trip("t2", "Malaga", "Spain", "2030-05-01", "2030-05-20"),
	}
	p := PlanTripSync(current, desired, Options{})

	if !p.IsNoop() {
		t.Fatalf("second run should be a no-op, got %+v", p)
	}
	if len(p.Unchanged) != 2 {
		t.Fatalf("want 2 unchanged, got %d", len(p.Unchanged))
	}
}

func TestPlanUpdatesShiftedDates(t *testing.T) {
	current := []nomads.Trip{trip("t1", "Malaga", "Spain", "2030-05-02", "2030-05-18")}
	desired := []nomads.DesiredTrip{want("Malaga", "Spain", "2030-05-01", "2030-05-20")}

	p := PlanTripSync(current, desired, Options{})

	if len(p.Updates) != 1 || len(p.Creates) != 0 {
		t.Fatalf("want 1 update and 0 creates, got %d/%d", len(p.Updates), len(p.Creates))
	}
	u := p.Updates[0]
	if u.Current.ID != "t1" {
		t.Fatalf("update lost the remote id: %q", u.Current.ID)
	}
	if u.Patch.StartDate == nil || u.Patch.EndDate == nil {
		t.Fatal("patch should carry both dates")
	}
	if u.Match != MatchPlaceAndOverlap {
		t.Fatalf("want overlap match, got %s", u.Match)
	}
}

// A trip that keeps its start date but changes its end date must patch only the
// end date, not recreate the trip.
func TestPlanMatchesOnStartDateAndPatchesEndOnly(t *testing.T) {
	current := []nomads.Trip{trip("t1", "Lisbon", "Portugal", "2026-03-01", "2026-03-10")}
	desired := []nomads.DesiredTrip{want("Lisbon", "Portugal", "2026-03-01", "2026-03-20")}

	p := PlanTripSync(current, desired, Options{})

	if len(p.Updates) != 1 {
		t.Fatalf("want 1 update, got %d", len(p.Updates))
	}
	u := p.Updates[0]
	if u.Patch.StartDate != nil {
		t.Error("start date should not be patched when it already matches")
	}
	if u.Patch.EndDate == nil || u.Patch.EndDate.String() != "2026-03-20" {
		t.Errorf("end date not patched correctly: %+v", u.Patch.EndDate)
	}
}

// Deletion must never happen unless explicitly asked for.
func TestPlanNeverDeletesByDefault(t *testing.T) {
	current := []nomads.Trip{
		trip("t1", "Bali", "Indonesia", "2024-01-01", "2024-02-01"),
		trip("t2", "Tokyo", "Japan", "2024-03-01", "2024-04-01"),
	}
	desired := []nomads.DesiredTrip{want("Lisbon", "Portugal", "2030-04-10", "2030-04-18")}

	p := PlanTripSync(current, desired, Options{})
	if len(p.Deletes) != 0 {
		t.Fatalf("default plan must not delete, got %d deletes", len(p.Deletes))
	}

	p2 := PlanTripSync(current, desired, Options{DeleteMissing: true})
	if len(p2.Deletes) != 2 {
		t.Fatalf("with DeleteMissing want 2 deletes, got %d", len(p2.Deletes))
	}
}

// The horizon protects travel history from a forward-looking desired state.
func TestPlanDeleteHorizonProtectsPastTrips(t *testing.T) {
	current := []nomads.Trip{
		trip("old", "Bali", "Indonesia", "2024-01-01", "2024-02-01"),
		trip("new", "Tokyo", "Japan", "2026-11-01", "2026-11-20"),
	}
	horizon := d("2026-01-01")
	p := PlanTripSync(current, nil, Options{DeleteMissing: true, DeleteHorizon: &horizon})

	if len(p.Deletes) != 1 || p.Deletes[0].ID != "new" {
		t.Fatalf("horizon should delete only the future trip, got %+v", p.Deletes)
	}
}

// Two remote trips to the same city overlapping the desired range cannot be
// told apart, so neither may be mutated.
func TestPlanReportsAmbiguousInsteadOfGuessing(t *testing.T) {
	current := []nomads.Trip{
		trip("a", "Malaga", "Spain", "2030-04-28", "2030-05-07"),
		trip("b", "Malaga", "Spain", "2030-05-09", "2030-05-22"),
	}
	desired := []nomads.DesiredTrip{want("Malaga", "Spain", "2030-05-01", "2030-05-20")}

	p := PlanTripSync(current, desired, Options{})

	if len(p.Ambiguous) != 1 {
		t.Fatalf("want 1 ambiguous match, got %d", len(p.Ambiguous))
	}
	if len(p.Ambiguous[0].Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(p.Ambiguous[0].Candidates))
	}
	// Crucially: no silent mutation of either candidate.
	if p.MutationCount() != 0 {
		t.Fatalf("ambiguous match must not produce mutations, got %d", p.MutationCount())
	}
}

// An exact match must win over a weaker overlapping candidate.
func TestPlanPrefersExactMatchOverOverlap(t *testing.T) {
	current := []nomads.Trip{
		trip("overlap", "Porto", "Portugal", "2026-05-02", "2026-05-09"),
		trip("exact", "Porto", "Portugal", "2026-05-01", "2026-05-10"),
	}
	desired := []nomads.DesiredTrip{want("Porto", "Portugal", "2026-05-01", "2026-05-10")}

	p := PlanTripSync(current, desired, Options{})

	if len(p.Unchanged) != 1 {
		t.Fatalf("want 1 unchanged, got %d (ambiguous=%d)", len(p.Unchanged), len(p.Ambiguous))
	}
	if p.Unchanged[0].Current.ID != "exact" {
		t.Fatalf("matched the wrong trip: %s", p.Unchanged[0].Current.ID)
	}
	// The other trip is untouched unless deletion was requested.
	if len(p.Deletes) != 0 {
		t.Fatalf("unexpected deletes: %+v", p.Deletes)
	}
}

// One remote trip must not be claimed by two desired trips.
func TestPlanDoesNotDoubleClaimRemoteTrip(t *testing.T) {
	current := []nomads.Trip{trip("t1", "Madrid", "Spain", "2026-06-01", "2026-06-30")}
	desired := []nomads.DesiredTrip{
		want("Madrid", "Spain", "2026-06-01", "2026-06-10"),
		want("Madrid", "Spain", "2026-06-20", "2026-06-30"),
	}

	p := PlanTripSync(current, desired, Options{})

	claimed := len(p.Updates) + len(p.Unchanged)
	if claimed != 1 {
		t.Fatalf("exactly one desired trip should claim the remote trip, got %d", claimed)
	}
	if len(p.Creates) != 1 {
		t.Fatalf("the other desired trip should be created, got %d creates", len(p.Creates))
	}
}

// Accents and casing must not cause a duplicate trip to be created.
func TestPlanMatchesAccentAndCaseInsensitively(t *testing.T) {
	current := []nomads.Trip{trip("t1", "Málaga", "Spain", "2029-02-03", "2029-02-11")}
	desired := []nomads.DesiredTrip{want("malaga", "spain", "2029-02-03", "2029-02-11")}

	p := PlanTripSync(current, desired, Options{})

	if !p.IsNoop() {
		t.Fatalf("accent/case differences must not cause mutations, got %+v", p)
	}
}

// Omitting the country should still match, and must not fabricate a change.
func TestPlanMatchesWhenCountryOmitted(t *testing.T) {
	current := []nomads.Trip{trip("t1", "Malaga", "Spain", "2030-05-01", "2030-05-20")}
	desired := []nomads.DesiredTrip{
		{City: "Malaga", StartDate: d("2030-05-01"), EndDate: d("2030-05-20")},
	}

	p := PlanTripSync(current, desired, Options{})

	if !p.IsNoop() {
		t.Fatalf("omitted country should match without changes, got %+v", p)
	}
}

func TestPlanWarnsOnGenuinelyOverlappingDesiredTrips(t *testing.T) {
	desired := []nomads.DesiredTrip{
		want("Rome", "Portugal", "2026-04-01", "2026-04-20"),
		want("Milan", "Portugal", "2026-04-10", "2026-04-25"),
	}
	p := PlanTripSync(nil, desired, Options{})

	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "overlap") {
		t.Fatalf("expected one overlap warning, got %v", p.Warnings)
	}
	// A warning must not block the creates.
	if len(p.Creates) != 2 {
		t.Fatalf("want 2 creates despite warning, got %d", len(p.Creates))
	}
}

// The planner must not reorder or otherwise mutate the caller's slices.
func TestPlanDoesNotMutateInputs(t *testing.T) {
	current := []nomads.Trip{
		trip("b", "Tokyo", "Japan", "2026-05-01", "2026-05-10"),
		trip("a", "Bali", "Indonesia", "2024-01-01", "2024-02-01"),
	}
	desired := []nomads.DesiredTrip{
		want("Zurich", "Switzerland", "2026-08-01", "2026-08-10"),
		want("Athens", "Greece", "2026-01-01", "2026-01-10"),
	}
	PlanTripSync(current, desired, Options{DeleteMissing: true})

	if current[0].ID != "b" || desired[0].City != "Zurich" {
		t.Fatal("planner reordered its caller's input slices")
	}
}

// Determinism: identical inputs must always produce an identical plan.
func TestPlanIsDeterministic(t *testing.T) {
	current := []nomads.Trip{
		trip("t1", "Lisbon", "Portugal", "2026-03-01", "2026-03-10"),
		trip("t2", "Porto", "Portugal", "2026-04-01", "2026-04-10"),
	}
	desired := []nomads.DesiredTrip{
		want("Lisbon", "Portugal", "2026-03-01", "2026-03-15"),
		want("Madrid", "Spain", "2026-05-01", "2026-05-10"),
	}
	first := Format(PlanTripSync(current, desired, Options{DeleteMissing: true}))
	for i := 0; i < 20; i++ {
		if got := Format(PlanTripSync(current, desired, Options{DeleteMissing: true})); got != first {
			t.Fatal("plan output is not deterministic across runs")
		}
	}
}

func TestValidateDesiredRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		trip nomads.DesiredTrip
	}{
		{"no city", nomads.DesiredTrip{StartDate: d("2026-01-01"), EndDate: d("2026-01-05")}},
		{"no start", nomads.DesiredTrip{City: "Rome", EndDate: d("2026-01-05")}},
		{"no end", nomads.DesiredTrip{City: "Rome", StartDate: d("2026-01-01")}},
		{"end before start", want("Rome", "Portugal", "2026-01-10", "2026-01-05")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDesired([]nomads.DesiredTrip{tc.trip})
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !nomads.IsCode(err, nomads.ErrInvalidInput) {
				t.Fatalf("want INVALID_INPUT, got %s", nomads.CodeOf(err))
			}
		})
	}
}

// A single-day trip (start == end) is legitimate.
func TestPlanAcceptsSingleDayTrip(t *testing.T) {
	desired := []nomads.DesiredTrip{want("Basel", "Switzerland", "2026-07-01", "2026-07-01")}
	if err := ValidateDesired(desired); err != nil {
		t.Fatalf("single-day trip should be valid: %v", err)
	}
	if p := PlanTripSync(nil, desired, Options{}); len(p.Creates) != 1 {
		t.Fatalf("want 1 create, got %d", len(p.Creates))
	}
}

func TestFormatRendersEmptySectionsAsNone(t *testing.T) {
	out := Format(PlanTripSync(nil, nil, Options{}))
	for _, s := range []string{"CREATE", "UPDATE", "UNCHANGED", "DELETE", "AMBIGUOUS"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing section %s", s)
		}
	}
	if strings.Count(out, "none") != 5 {
		t.Errorf("expected all 5 sections empty:\n%s", out)
	}
}
