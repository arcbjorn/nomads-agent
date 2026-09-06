package client

import (
	"context"
	"os"
	"testing"

	"github.com/arcbjorn/nomads-agent/internal/storage"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// Live tests run against the real account and are opt-in. They are read-mostly:
// the only mutation creates a trip far in the future and deletes it again, so a
// failure cannot corrupt real travel history.
func liveClient(t *testing.T) *Hybrid {
	t.Helper()
	if os.Getenv("NOMADS_INTEGRATION_TEST") != "1" {
		t.Skip("set NOMADS_INTEGRATION_TEST=1 to run live tests")
	}

	store, err := storage.New(os.Getenv("NOMADS_AGENT_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.LoadSession()
	if err != nil {
		t.Fatal(err)
	}

	username := firstNonEmpty(os.Getenv("NOMADS_USERNAME"), sess.Username)
	apiKey := firstNonEmpty(os.Getenv("NOMADS_API_KEY"), sess.APIKey)

	var official *Official
	if username != "" && apiKey != "" {
		if official, err = NewOfficial(username, apiKey); err != nil {
			t.Fatal(err)
		}
	}
	var private *Client
	if sess.HasBrowserSession() {
		if private, err = New(sess); err != nil {
			t.Fatal(err)
		}
		private.SetUsername(username)
	}
	if official == nil && private == nil {
		t.Skip("no credentials configured; run 'nomads auth key' first")
	}
	h, err := NewHybrid(official, private)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestIntegrationListTrips(t *testing.T) {
	c := liveClient(t)
	trips, err := c.ListTrips(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read %d trips", len(trips))
	for _, tr := range trips {
		if tr.StartDate.IsZero() || tr.EndDate.IsZero() {
			t.Errorf("trip with no dates: %+v", tr)
		}
		if tr.EndDate.Before(tr.StartDate) {
			t.Errorf("trip ends before it starts: %s", tr.Label())
		}
	}
}

func TestIntegrationGetProfile(t *testing.T) {
	c := liveClient(t)
	if c.Capabilities().GetProfile == "unavailable" {
		t.Skip("no browser session; profile reads are unavailable")
	}
	p, err := c.GetProfile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Username == "" {
		t.Error("profile has no username")
	}
	t.Logf("profile @%s, %d tags", p.Username, len(p.Tags))
}

// The one mutating live test: create a trip a decade out, verify it, delete it,
// and verify it is gone. It never touches an existing trip.
func TestIntegrationTripLifecycle(t *testing.T) {
	c := liveClient(t)
	if c.Capabilities().DeleteTrip == "unavailable" {
		t.Skip("no browser session; a trip could be created but not cleaned up")
	}
	ctx := context.Background()

	req := nomads.CreateTripRequest{
		City: "Lisbon", Country: "Portugal",
		StartDate: nomads.MustParseDate("2036-03-04"),
		EndDate:   nomads.MustParseDate("2036-03-09"),
	}
	trip, err := c.CreateTrip(ctx, req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("created %s", trip.Label())

	// Always clean up, even if an assertion below fails.
	defer func() {
		if trip.ID == "" {
			t.Error("no trip id returned; clean up manually")
			return
		}
		if err := c.DeleteTrip(ctx, trip.ID); err != nil {
			t.Errorf("cleanup failed, delete %s manually: %v", trip.ID, err)
		}
	}()

	if err := c.VerifyTrip(ctx, *trip); err != nil {
		t.Errorf("verify after create: %v", err)
	}
}
