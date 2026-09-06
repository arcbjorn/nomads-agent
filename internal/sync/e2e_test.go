package sync

import (
	"strings"
	"testing"

	"github.com/arcbjorn/nomads-agent/internal/trips"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// The README scenario end to end: parse the itinerary file, plan against a
// partially-correct remote, and render the dry-run report.
func TestReadmeScenarioEndToEnd(t *testing.T) {
	desired, err := trips.ParseYAML(`
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

	remote := []nomads.Trip{
		// Already correct.
		{ID: "t1", City: "Porto", Country: "Portugal",
			StartDate: d("2030-04-18"), EndDate: d("2030-05-01")},
		// Dates drifted.
		{ID: "t2", City: "Malaga", Country: "Spain",
			StartDate: d("2030-05-02"), EndDate: d("2030-05-18")},
	}

	plan := PlanTripSync(remote, desired, Options{})

	if len(plan.Creates) != 1 || plan.Creates[0].City != "Lisbon" {
		t.Errorf("want 1 create (Lisbon), got %+v", plan.Creates)
	}
	if len(plan.Updates) != 1 || plan.Updates[0].Current.ID != "t2" {
		t.Errorf("want 1 update (Malaga), got %+v", plan.Updates)
	}
	if len(plan.Unchanged) != 1 || plan.Unchanged[0].Current.City != "Porto" {
		t.Errorf("want Porto unchanged, got %+v", plan.Unchanged)
	}
	if len(plan.Deletes) != 0 {
		t.Error("nothing should be deleted without --delete-missing")
	}

	out := Format(plan)
	for _, want := range []string{"CREATE", "Lisbon", "UPDATE", "Malaga",
		"2030-05-20", "UNCHANGED", "Porto", "DELETE", "none"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q", want)
		}
	}
	t.Logf("\n%s", out)
}
