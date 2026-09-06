package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// officialStub serves the documented API shape.
func officialStub(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *Official {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(h))
	t.Cleanup(srv.Close)
	o, err := NewOfficial("testuser", "testkey", WithOfficialBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestOfficialListTripsParsesArray(t *testing.T) {
	o := officialStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "testkey" {
			t.Errorf("key not sent")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"city":"Porto","country":"Portugal","city_slug":"porto-portugal",
			 "date_start":"2030-05-01","date_end":"2030-05-20"},
			{"city":"Lisbon","country":"Portugal","city_slug":"lisbon-portugal",
			 "date_start":"2030-04-10","date_end":"2030-04-18"}]`))
	})

	trips, err := o.ListTrips(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(trips) != 2 {
		t.Fatalf("want 2 trips, got %d", len(trips))
	}
	// Must come back chronologically sorted.
	if trips[0].City != "Lisbon" {
		t.Errorf("not sorted: first is %s", trips[0].City)
	}
	if trips[0].StartDate.String() != "2030-04-10" {
		t.Errorf("bad date: %s", trips[0].StartDate)
	}
}

// The API has returned both a bare array and a wrapped object; accept both.
func TestOfficialListTripsAcceptsWrappedObject(t *testing.T) {
	o := officialStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trips":[{"city":"Lisbon","country":"Portugal",
			"date_start":"2026-03-01","date_end":"2026-03-10"}]}`))
	})
	trips, err := o.ListTrips(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(trips) != 1 || trips[0].City != "Lisbon" {
		t.Fatalf("got %+v", trips)
	}
}

func TestOfficialInvalidKeyBecomesAuthExpired(t *testing.T) {
	o := officialStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The real API returns this with HTTP 200.
		_, _ = w.Write([]byte(`{"error":"invalid_username_or_key",
			"detail":"Get your key at https://nomads.com/settings"}`))
	})
	_, err := o.ListTrips(context.Background())
	if !nomads.IsCode(err, nomads.ErrAuthExpired) {
		t.Fatalf("want AUTH_EXPIRED, got %s (%v)", nomads.CodeOf(err), err)
	}
}

func TestOfficialCreateTripSendsSlugOrCoordinates(t *testing.T) {
	var got map[string]any
	o := officialStub(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"city":"Porto","country":"Portugal",
			"date_start":"2030-05-01","date_end":"2030-05-20"}`))
	})

	// A slug is passed straight through.
	_, err := o.CreateTrip(context.Background(), nomads.CreateTripRequest{
		Slug: "porto-portugal", City: "Porto",
		StartDate: nomads.MustParseDate("2030-05-01"),
		EndDate:   nomads.MustParseDate("2030-05-20"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["city_slug"] != "porto-portugal" {
		t.Errorf("city_slug not sent: %v", got)
	}
	if _, present := got["latitude"]; present {
		t.Error("latitude must not be sent alongside a slug")
	}
	if got["date_start"] != "2030-05-01" {
		t.Errorf("dates not ISO: %v", got["date_start"])
	}
}

func TestOfficialCreateTripRequiresAPlace(t *testing.T) {
	o := officialStub(t, func(w http.ResponseWriter, r *http.Request) {})
	_, err := o.CreateTrip(context.Background(), nomads.CreateTripRequest{
		City:      "Nowhere",
		StartDate: nomads.MustParseDate("2030-05-01"),
		EndDate:   nomads.MustParseDate("2030-05-20"),
	})
	if !nomads.IsCode(err, nomads.ErrInvalidInput) {
		t.Fatalf("want INVALID_INPUT without slug/coords, got %s", nomads.CodeOf(err))
	}
}

func TestOfficialRateLimitIsSemantic(t *testing.T) {
	o := officialStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := o.ListTrips(context.Background())
	if !nomads.IsCode(err, nomads.ErrRateLimited) {
		t.Fatalf("want RATE_LIMITED, got %s", nomads.CodeOf(err))
	}
}

// Operations with no official endpoint must fail with a clear explanation
// rather than silently doing nothing.
func TestHybridReportsUnsupportedOperations(t *testing.T) {
	o := officialStub(t, func(w http.ResponseWriter, r *http.Request) {})
	h, err := NewHybrid(o, nil) // official only, no browser session
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.UpdateTrip(context.Background(), "id", nomads.TripPatch{}); err == nil {
		t.Error("UpdateTrip should be unsupported without the private client")
	}
	if err := h.DeleteTrip(context.Background(), "id"); err == nil {
		t.Error("DeleteTrip should be unsupported without the private client")
	}
	if _, err := h.GetProfile(context.Background()); err == nil {
		t.Error("GetProfile should be unsupported without the private client")
	}

	caps := h.Capabilities()
	if caps.ListTrips != "official-api" {
		t.Errorf("list_trips should use the official API, got %s", caps.ListTrips)
	}
	if caps.UpdateTrip != "unavailable" || caps.UpdateProfile != "unavailable" {
		t.Errorf("unsupported ops should report unavailable: %+v", caps)
	}
}

func TestHybridCapabilitiesWithBothBackends(t *testing.T) {
	o := officialStub(t, func(w http.ResponseWriter, r *http.Request) {})
	_, srv := newStub(t)
	p := newTestClient(t, srv)

	h, err := NewHybrid(o, p)
	if err != nil {
		t.Fatal(err)
	}
	caps := h.Capabilities()
	if caps.ListTrips != "official-api" || caps.CreateTrip != "official-api" {
		t.Errorf("trips reads/appends should prefer the official API: %+v", caps)
	}
	if caps.UpdateTrip != "private-api" || caps.GetProfile != "private-api" {
		t.Errorf("unofficial-only ops should use the private client: %+v", caps)
	}
}

// A broken official API must not strand the user when a session exists.
func TestHybridFallsBackWhenOfficialFails(t *testing.T) {
	o := officialStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"invalid_username_or_key","detail":"bad key"}`))
	})
	_, srv := newStub(t)
	p := newTestClient(t, srv)

	h, err := NewHybrid(o, p)
	if err != nil {
		t.Fatal(err)
	}
	trips, err := h.ListTrips(context.Background())
	if err != nil {
		t.Fatalf("should have fallen back to the private client: %v", err)
	}
	if len(trips) != 3 {
		t.Fatalf("want the 3 fixture trips, got %d", len(trips))
	}
}

// A rate limit is not worth retrying on the other backend.
func TestHybridDoesNotFallBackOnRateLimit(t *testing.T) {
	if shouldFallBack(nomads.Errorf(nomads.ErrRateLimited, "slow down")) {
		t.Error("a rate limit must not trigger a fallback")
	}
	if !shouldFallBack(nomads.Errorf(nomads.ErrAuthExpired, "bad key")) {
		t.Error("an auth failure should trigger a fallback")
	}
}

func TestNewHybridRequiresABackend(t *testing.T) {
	if _, err := NewHybrid(nil, nil); err == nil {
		t.Fatal("expected an error when no backend is configured")
	}
}

// The official trips fixture must parse into the domain model unchanged.
func TestOfficialListTripsFixture(t *testing.T) {
	body := mustRead(t, "../../fixtures/trips.json")
	o := officialStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})

	trips, err := o.ListTrips(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(trips) != 3 {
		t.Fatalf("want 3 trips, got %d", len(trips))
	}
	// Sorted chronologically regardless of the order in the payload.
	if trips[0].City != "Málaga" {
		t.Errorf("not sorted: first is %q", trips[0].City)
	}
	if trips[0].Slug != "malaga-spain" || trips[0].Latitude == 0 {
		t.Errorf("fields lost: %+v", trips[0])
	}
	for _, tr := range trips {
		if tr.ID == "" || tr.StartDate.IsZero() || tr.EndDate.IsZero() {
			t.Errorf("incomplete trip: %+v", tr)
		}
	}
}
