package client

import (
	"context"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// Nomads is the full capability surface this tool exposes.
type Nomads interface {
	GetProfile(ctx context.Context) (*nomads.Profile, error)
	UpdateProfile(ctx context.Context, patch nomads.ProfilePatch) (*nomads.Profile, error)
	ListTrips(ctx context.Context) ([]nomads.Trip, error)
	CreateTrip(ctx context.Context, req nomads.CreateTripRequest) (*nomads.Trip, error)
	UpdateTrip(ctx context.Context, id string, patch nomads.TripPatch) (*nomads.Trip, error)
	DeleteTrip(ctx context.Context, id string) error
}

// Hybrid routes each operation to the best backend available.
//
// Nomads.com's documented API (https://nomads.com/api) covers reading trip
// history and appending a trip. It has no endpoint for editing or removing a
// trip, and none for profile fields. Where the official API can do the job it
// is used, because it is supported and versioned; the reverse-engineered
// first-party client fills in only the operations it cannot express.
type Hybrid struct {
	// official is the documented API. Nil when no API key is configured.
	official *Official
	// private is the reverse-engineered first-party client. Nil when there is
	// no browser session.
	private *Client
}

// NewHybrid builds a client from whichever backends are configured. At least
// one must be non-nil.
func NewHybrid(official *Official, private *Client) (*Hybrid, error) {
	if official == nil && private == nil {
		return nil, nomads.Errorf(nomads.ErrAuthExpired,
			"not configured; run 'nomads auth login' with an API key from https://nomads.com/settings")
	}
	return &Hybrid{official: official, private: private}, nil
}

// Capabilities describes which operations are currently available.
type Capabilities struct {
	ListTrips     string `json:"list_trips"`
	CreateTrip    string `json:"create_trip"`
	UpdateTrip    string `json:"update_trip"`
	DeleteTrip    string `json:"delete_trip"`
	GetProfile    string `json:"get_profile"`
	UpdateProfile string `json:"update_profile"`
}

// Capabilities reports the backend backing each operation, or "unavailable".
func (h *Hybrid) Capabilities() Capabilities {
	// Trips can be read and appended through the documented API; everything
	// else exists only on the first-party endpoints.
	officialOrPrivate := "unavailable"
	switch {
	case h.official != nil:
		officialOrPrivate = "official-api"
	case h.private != nil:
		officialOrPrivate = "private-api"
	}
	privateOnly := "unavailable"
	if h.private != nil {
		privateOnly = "private-api"
	}
	return Capabilities{
		ListTrips:     officialOrPrivate,
		CreateTrip:    officialOrPrivate,
		UpdateTrip:    privateOnly,
		DeleteTrip:    privateOnly,
		GetProfile:    privateOnly,
		UpdateProfile: privateOnly,
	}
}

// ListTrips prefers the official API.
func (h *Hybrid) ListTrips(ctx context.Context) ([]nomads.Trip, error) {
	if h.official != nil {
		trips, err := h.official.ListTrips(ctx)
		// Fall back only when the official path is unusable, not when it
		// legitimately reports an empty history.
		if err == nil {
			return trips, nil
		}
		if h.private == nil || !shouldFallBack(err) {
			return nil, err
		}
	}
	if h.private == nil {
		return nil, unsupported("list trips")
	}
	return h.private.ListTrips(ctx)
}

// CreateTrip prefers the official API.
func (h *Hybrid) CreateTrip(ctx context.Context, req nomads.CreateTripRequest) (*nomads.Trip, error) {
	if h.official != nil {
		// The official endpoint needs a slug or coordinates; resolve a bare city
		// name through the private client's geocoder when that is all we have.
		if req.Slug == "" && req.Latitude == 0 && req.Longitude == 0 && h.private != nil {
			if place, err := h.private.geocoder.Geocode(ctx, req.City, req.Country); err == nil {
				req.Latitude, req.Longitude = place.Latitude, place.Longitude
				if req.Country == "" {
					req.Country = place.Country
				}
			}
		}
		trip, err := h.official.CreateTrip(ctx, req)
		if err == nil {
			return trip, nil
		}
		if h.private == nil || !shouldFallBack(err) {
			return nil, err
		}
	}
	if h.private == nil {
		return nil, unsupported("add a trip")
	}
	return h.private.CreateTrip(ctx, req)
}

// UpdateTrip has no official endpoint, so it requires the private client.
func (h *Hybrid) UpdateTrip(ctx context.Context, id string, patch nomads.TripPatch) (*nomads.Trip, error) {
	if h.private == nil {
		return nil, unsupported("edit a trip")
	}
	return h.private.UpdateTrip(ctx, id, patch)
}

// DeleteTrip has no official endpoint, so it requires the private client.
func (h *Hybrid) DeleteTrip(ctx context.Context, id string) error {
	if h.private == nil {
		return unsupported("delete a trip")
	}
	return h.private.DeleteTrip(ctx, id)
}

// GetProfile has no official endpoint, so it requires the private client.
func (h *Hybrid) GetProfile(ctx context.Context) (*nomads.Profile, error) {
	if h.private == nil {
		return nil, unsupported("read the profile")
	}
	return h.private.GetProfile(ctx)
}

// UpdateProfile has no official endpoint, so it requires the private client.
func (h *Hybrid) UpdateProfile(ctx context.Context, patch nomads.ProfilePatch) (*nomads.Profile, error) {
	if h.private == nil {
		return nil, unsupported("update the profile")
	}
	return h.private.UpdateProfile(ctx, patch)
}

// VerifyTrip re-reads a trip and confirms it matches what was requested.
func (h *Hybrid) VerifyTrip(ctx context.Context, want nomads.Trip) error {
	trips, err := h.ListTrips(ctx)
	if err != nil {
		return err
	}
	for _, t := range trips {
		// The official API may not echo an id, so fall back to matching on the
		// place and dates, which is what the caller actually asked for.
		if (want.ID != "" && t.ID == want.ID) ||
			(t.Place() == want.Place() &&
				t.StartDate.Compare(want.StartDate) == 0 &&
				t.EndDate.Compare(want.EndDate) == 0) {
			return nil
		}
	}
	return nomads.Errorf(nomads.ErrRemoteStateMismatch,
		"trip was written but is absent from the trip list").
		WithDetail("expected", want.Label())
}

// shouldFallBack reports whether an official-API failure is worth retrying on
// the private client. Auth and availability problems are; a rejected request
// or a rate limit would fail the same way twice.
func shouldFallBack(err error) bool {
	switch nomads.CodeOf(err) {
	case nomads.ErrAuthExpired, nomads.ErrAPIChanged, nomads.ErrNetwork:
		return true
	}
	return false
}

// unsupported explains that an operation needs the browser-session backend.
func unsupported(what string) error {
	return nomads.Errorf(nomads.ErrInvalidInput,
		"cannot %s: the official Nomads.com API has no endpoint for this, and no "+
			"browser session is configured. Run 'nomads auth login --link <magic link>' "+
			"to enable it.", what)
}
