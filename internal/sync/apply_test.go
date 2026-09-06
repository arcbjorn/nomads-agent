package sync

import (
	"context"
	"fmt"
	"testing"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// fakeClient is an in-memory stand-in for Nomads.com that reproduces the two
// behaviours that matter: ids are assigned by the server, and an update
// reassigns the id.
type fakeClient struct {
	trips  []nomads.Trip
	nextID int
	// failCreate makes the named city fail, to test partial-failure handling.
	failCreate string
	creates    int
	updates    int
	deletes    int
}

func (f *fakeClient) ListTrips(context.Context) ([]nomads.Trip, error) {
	out := append([]nomads.Trip(nil), f.trips...)
	nomads.SortTrips(out)
	return out, nil
}

func (f *fakeClient) CreateTrip(_ context.Context, req nomads.CreateTripRequest) (*nomads.Trip, error) {
	f.creates++
	if req.City == f.failCreate {
		return nil, nomads.Errorf(nomads.ErrGeocodeFailed, "no coordinates for %q", req.City)
	}
	f.nextID++
	t := nomads.Trip{
		ID: fmt.Sprintf("id%d", f.nextID), City: req.City, Country: req.Country,
		StartDate: req.StartDate, EndDate: req.EndDate, Note: req.Note,
	}
	f.trips = append(f.trips, t)
	return &t, nil
}

func (f *fakeClient) UpdateTrip(_ context.Context, id string, patch nomads.TripPatch) (*nomads.Trip, error) {
	f.updates++
	for i, t := range f.trips {
		if t.ID != id {
			continue
		}
		if patch.StartDate != nil {
			t.StartDate = *patch.StartDate
		}
		if patch.EndDate != nil {
			t.EndDate = *patch.EndDate
		}
		if patch.Country != nil {
			t.Country = *patch.Country
		}
		if patch.Note != nil {
			t.Note = *patch.Note
		}
		// Mirror the real API: editing reassigns the id.
		f.nextID++
		t.ID = fmt.Sprintf("id%d", f.nextID)
		f.trips[i] = t
		return &t, nil
	}
	return nil, nomads.Errorf(nomads.ErrTripNotFound, "no trip %s", id)
}

func (f *fakeClient) DeleteTrip(_ context.Context, id string) error {
	f.deletes++
	for i, t := range f.trips {
		if t.ID == id {
			f.trips = append(f.trips[:i], f.trips[i+1:]...)
			return nil
		}
	}
	return nomads.Errorf(nomads.ErrTripNotFound, "no trip %s", id)
}

// The headline agent scenario, run twice: the second run must do nothing.
func TestSyncIsIdempotentAcrossRuns(t *testing.T) {
	desired := []nomads.DesiredTrip{
		want("Lisbon", "Portugal", "2030-04-10", "2030-04-18"),
		want("Porto", "Portugal", "2030-04-18", "2030-05-01"),
		want("Malaga", "Spain", "2030-05-01", "2030-05-20"),
	}
	f := &fakeClient{}
	ctx := context.Background()

	res1, _, err := Sync(ctx, f, desired, Options{})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(res1.Created) != 3 {
		t.Fatalf("first run should create 3 trips, got %d", len(res1.Created))
	}

	// Second run: identical desired state, so zero mutations.
	createsBefore, updatesBefore, deletesBefore := f.creates, f.updates, f.deletes
	res2, plan2, err := Sync(ctx, f, desired, Options{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !plan2.IsNoop() {
		t.Fatalf("second run should be a no-op, got plan %+v", plan2)
	}
	if f.creates != createsBefore || f.updates != updatesBefore || f.deletes != deletesBefore {
		t.Fatalf("second run mutated remote state: +%d creates +%d updates +%d deletes",
			f.creates-createsBefore, f.updates-updatesBefore, f.deletes-deletesBefore)
	}
	if len(res2.Unchanged) != 3 {
		t.Fatalf("want 3 unchanged, got %d", len(res2.Unchanged))
	}
}

func TestSyncAppliesUpdatesAndVerifies(t *testing.T) {
	f := &fakeClient{trips: []nomads.Trip{
		{ID: "id1", City: "Malaga", Country: "Spain",
			StartDate: d("2030-05-02"), EndDate: d("2030-05-18")},
	}, nextID: 1}

	desired := []nomads.DesiredTrip{want("Malaga", "Spain", "2030-05-01", "2030-05-20")}
	res, _, err := Sync(context.Background(), f, desired, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updated) != 1 {
		t.Fatalf("want 1 update, got %d", len(res.Updated))
	}
	// The result must carry the reassigned id, not the stale one.
	if res.Updated[0].ID == "id1" {
		t.Error("result kept the stale trip id after an update")
	}
	if f.trips[0].StartDate.String() != "2030-05-01" {
		t.Errorf("start date not applied: %s", f.trips[0].StartDate)
	}
}

// One failing create must not abort the others, and must be reported.
func TestApplyContinuesPastPartialFailure(t *testing.T) {
	f := &fakeClient{failCreate: "Nowhere"}
	plan := PlanTripSync(nil, []nomads.DesiredTrip{
		want("Lisbon", "Portugal", "2030-04-10", "2030-04-18"),
		want("Nowhere", "Atlantis", "2030-04-20", "2030-05-02"),
		want("Malaga", "Spain", "2026-10-06", "2030-05-20"),
	}, Options{})

	res, err := Apply(context.Background(), f, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 2 {
		t.Fatalf("want 2 successful creates, got %d", len(res.Created))
	}
	if len(res.Errors) != 1 {
		t.Fatalf("want 1 recorded error, got %d", len(res.Errors))
	}
	if res.Errors[0].Code != nomads.ErrGeocodeFailed {
		t.Errorf("wrong error code: %s", res.Errors[0].Code)
	}
	if !res.HasErrors() {
		t.Error("HasErrors should be true")
	}
}

func TestSyncNeverDeletesWithoutOptIn(t *testing.T) {
	f := &fakeClient{trips: []nomads.Trip{
		{ID: "old", City: "Bali", Country: "Indonesia",
			StartDate: d("2024-01-01"), EndDate: d("2024-02-01")},
	}, nextID: 1}

	desired := []nomads.DesiredTrip{want("Lisbon", "Portugal", "2030-04-10", "2030-04-18")}
	if _, _, err := Sync(context.Background(), f, desired, Options{}); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 0 {
		t.Fatalf("sync deleted %d trips without --delete-missing", f.deletes)
	}
	if len(f.trips) != 2 {
		t.Fatalf("expected the old trip to survive, have %d trips", len(f.trips))
	}
}

func TestSyncDeletesWhenOptedIn(t *testing.T) {
	f := &fakeClient{trips: []nomads.Trip{
		{ID: "old", City: "Bali", Country: "Indonesia",
			StartDate: d("2026-01-01"), EndDate: d("2026-02-01")},
	}, nextID: 1}

	desired := []nomads.DesiredTrip{want("Lisbon", "Portugal", "2030-04-10", "2030-04-18")}
	res, _, err := Sync(context.Background(), f, desired, Options{DeleteMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0].City != "Bali" {
		t.Fatalf("expected Bali to be deleted, got %+v", res.Deleted)
	}
}

// Ambiguous matches must be surfaced and must not mutate anything.
func TestSyncSurfacesAmbiguousWithoutMutating(t *testing.T) {
	f := &fakeClient{trips: []nomads.Trip{
		{ID: "a", City: "Malaga", Country: "Spain", StartDate: d("2030-04-28"), EndDate: d("2030-05-07")},
		{ID: "b", City: "Malaga", Country: "Spain", StartDate: d("2030-05-09"), EndDate: d("2030-05-22")},
	}, nextID: 2}

	desired := []nomads.DesiredTrip{want("Malaga", "Spain", "2030-05-01", "2030-05-20")}
	res, _, err := Sync(context.Background(), f, desired, Options{})
	// Verification legitimately fails, because the ambiguity was not resolved.
	if err != nil && !nomads.IsCode(err, nomads.ErrRemoteStateMismatch) {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Ambiguous) != 1 {
		t.Fatalf("want 1 ambiguous match, got %d", len(res.Ambiguous))
	}
	if f.creates+f.updates+f.deletes != 0 {
		t.Fatal("an ambiguous match must not cause any mutation")
	}
}

func TestVerifyDetectsDivergence(t *testing.T) {
	f := &fakeClient{trips: []nomads.Trip{
		{ID: "id1", City: "Malaga", Country: "Spain",
			StartDate: d("2030-05-02"), EndDate: d("2030-05-18")},
	}}
	err := Verify(context.Background(), f, []nomads.DesiredTrip{
		want("Malaga", "Spain", "2030-05-01", "2030-05-20"),
	})
	if !nomads.IsCode(err, nomads.ErrRemoteStateMismatch) {
		t.Fatalf("want REMOTE_STATE_MISMATCH, got %s", nomads.CodeOf(err))
	}
}

func TestSyncRejectsInvalidDesiredState(t *testing.T) {
	f := &fakeClient{}
	_, _, err := Sync(context.Background(), f, []nomads.DesiredTrip{
		{City: "Rome", StartDate: d("2026-01-10"), EndDate: d("2026-01-05")},
	}, Options{})
	if !nomads.IsCode(err, nomads.ErrInvalidInput) {
		t.Fatalf("want INVALID_INPUT, got %s", nomads.CodeOf(err))
	}
	if f.creates != 0 {
		t.Fatal("invalid input must be rejected before any mutation")
	}
}
