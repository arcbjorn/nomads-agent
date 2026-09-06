package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	nsync "github.com/arcbjorn/nomads-agent/internal/sync"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// asNomads is errors.As specialised to *nomads.Error.
func asNomads(err error, target **nomads.Error) bool { return errors.As(err, target) }

// tripArgSchema is the shape of one desired trip in tool arguments.
var tripArgSchema = obj("One stay in one city", map[string]schema{
	"city":    str("City name, e.g. Lisbon"),
	"country": str("Country name, e.g. Portugal"),
	"from":    str("Start date, YYYY-MM-DD"),
	"to":      str("End date, YYYY-MM-DD"),
	"slug":    str("Optional Nomads.com city slug, e.g. lisbon-portugal"),
	"note":    str("Optional note"),
}, "city", "from", "to")

// tools lists every tool this server exposes.
func tools() []tool {
	return []tool{
		{
			Name: "nomads_get_profile",
			Description: "Read the authenticated Nomads.com profile: bio, tags, website and " +
				"social links. Requires a browser session (the official API has no profile endpoint).",
			InputSchema: obj("", map[string]schema{}),
		},
		{
			Name: "nomads_update_profile",
			Description: "Update profile fields. Only the fields supplied are changed; " +
				"omitted fields are left untouched. Tags replace the whole tag set. " +
				"The result is re-read from Nomads.com to verify the change took effect.",
			InputSchema: obj("", map[string]schema{
				"bio":       str("Profile bio. Cannot be blank; Nomads.com rejects an empty bio."),
				"tags":      arrayOf(str("A tag, e.g. 'Web Dev'"), "Replaces the entire tag set"),
				"website":   str("Website URL"),
				"instagram": str("Instagram handle"),
				"youtube":   str("YouTube URL"),
				"tiktok":    str("TikTok handle"),
				"twitter":   str("Twitter/X handle"),
			}),
		},
		{
			Name: "nomads_list_trips",
			Description: "List the member's trips, past and upcoming, sorted by start date. " +
				"Each trip carries an id, city, country, dates and status.",
			InputSchema: obj("", map[string]schema{}),
		},
		{
			Name: "nomads_add_trip",
			Description: "Add one trip. The city is resolved to a Nomads.com city by slug or " +
				"by geocoding its name. The write is verified by re-reading the trip list.",
			InputSchema: obj("", map[string]schema{
				"city":    str("City name, e.g. Lisbon"),
				"country": str("Country name, e.g. Portugal"),
				"from":    str("Start date, YYYY-MM-DD"),
				"to":      str("End date, YYYY-MM-DD"),
				"slug":    str("Optional Nomads.com city slug, e.g. lisbon-portugal"),
				"note":    str("Optional note"),
			}, "city", "from", "to"),
		},
		{
			Name: "nomads_update_trip",
			Description: "Change an existing trip's dates, city or note. Requires a browser " +
				"session. Note that Nomads.com assigns a NEW trip id on edit; the new id is returned.",
			InputSchema: obj("", map[string]schema{
				"id":      str("Trip id, from nomads_list_trips"),
				"city":    str("New city name"),
				"country": str("New country name"),
				"from":    str("New start date, YYYY-MM-DD"),
				"to":      str("New end date, YYYY-MM-DD"),
				"note":    str("New note"),
			}, "id"),
		},
		{
			Name: "nomads_delete_trip",
			Description: "Delete one trip by id. Requires a browser session. The deletion is " +
				"verified by re-reading the trip list, because the API reports success even for an unknown id.",
			InputSchema: obj("", map[string]schema{
				"id": str("Trip id, from nomads_list_trips"),
			}, "id"),
		},
		{
			Name: "nomads_plan_trip_sync",
			Description: "Preview the changes needed to make Nomads.com match a desired " +
				"itinerary. This NEVER mutates anything: use it to show the user what would happen. " +
				"Returns creates, updates, deletes, unchanged and any ambiguous matches.",
			InputSchema: obj("", map[string]schema{
				"trips":          arrayOf(tripArgSchema, "The complete desired itinerary"),
				"delete_missing": boolean("Include deletions for remote trips not in the list (default false)"),
				"delete_after":   str("With delete_missing, only delete trips starting on or after this date (YYYY-MM-DD)"),
			}, "trips"),
		},
		{
			Name: "nomads_sync_trips",
			Description: "Reconcile Nomads.com with a desired itinerary, making only the " +
				"necessary changes. Running it twice with the same input changes nothing the " +
				"second time. Deletions require delete_missing: true. Ambiguous matches are " +
				"reported, never guessed. Verifies the result after writing.",
			InputSchema: obj("", map[string]schema{
				"trips":          arrayOf(tripArgSchema, "The complete desired itinerary"),
				"delete_missing": boolean("Delete remote trips absent from the list (destructive, default false)"),
				"delete_after":   str("With delete_missing, only delete trips starting on or after this date (YYYY-MM-DD)"),
			}, "trips"),
		},
		{
			Name: "nomads_capabilities",
			Description: "Report which operations are currently available and which backend " +
				"serves them. Use this to explain why an operation is unsupported.",
			InputSchema: obj("", map[string]schema{}),
		},
	}
}

// syncArgs is the argument shape for the two sync tools.
type syncArgs struct {
	Trips         []tripArg `json:"trips"`
	DeleteMissing bool      `json:"delete_missing"`
	DeleteAfter   string    `json:"delete_after"`
}

// tripArg is one desired trip as an agent supplies it.
type tripArg struct {
	City    string `json:"city"`
	Country string `json:"country"`
	From    string `json:"from"`
	To      string `json:"to"`
	Slug    string `json:"slug"`
	Note    string `json:"note"`
}

// toDesired converts and validates agent-supplied trips.
func toDesired(args []tripArg) ([]nomads.DesiredTrip, error) {
	out := make([]nomads.DesiredTrip, 0, len(args))
	for i, a := range args {
		start, err := nomads.ParseDate(a.From)
		if err != nil {
			return nil, nomads.Errorf(nomads.ErrInvalidInput,
				"trip %d (%s): 'from' %s", i+1, a.City, err)
		}
		end, err := nomads.ParseDate(a.To)
		if err != nil {
			return nil, nomads.Errorf(nomads.ErrInvalidInput,
				"trip %d (%s): 'to' %s", i+1, a.City, err)
		}
		d := nomads.DesiredTrip{
			City: strings.TrimSpace(a.City), Country: strings.TrimSpace(a.Country),
			StartDate: start, EndDate: end, Note: a.Note, Slug: strings.TrimSpace(a.Slug),
		}
		if err := d.Validate(); err != nil {
			return nil, nomads.Wrap(err, nomads.ErrInvalidInput, "trip %d: %s", i+1, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// syncOptions builds planner options from agent arguments.
func syncOptions(a syncArgs) (nsync.Options, error) {
	opts := nsync.Options{DeleteMissing: a.DeleteMissing}
	if strings.TrimSpace(a.DeleteAfter) != "" {
		d, err := nomads.ParseDate(a.DeleteAfter)
		if err != nil {
			return opts, nomads.Errorf(nomads.ErrInvalidInput, "delete_after: %s", err)
		}
		opts.DeleteHorizon = &d
	}
	return opts, nil
}

// dispatch runs one tool by name.
func (s *Server) dispatch(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	c, err := s.clientOrErr()
	if err != nil {
		return "", err
	}

	switch name {
	case "nomads_capabilities":
		return jsonResult(c.Capabilities())

	case "nomads_get_profile":
		p, err := c.GetProfile(ctx)
		if err != nil {
			return "", err
		}
		return jsonResult(p)

	case "nomads_update_profile":
		var a struct {
			Bio       *string   `json:"bio"`
			Tags      *[]string `json:"tags"`
			Website   *string   `json:"website"`
			Instagram *string   `json:"instagram"`
			YouTube   *string   `json:"youtube"`
			TikTok    *string   `json:"tiktok"`
			Twitter   *string   `json:"twitter"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", nomads.Wrap(err, nomads.ErrInvalidInput, "invalid arguments")
		}
		patch := nomads.ProfilePatch{
			Bio: a.Bio, Website: a.Website, Instagram: a.Instagram,
			YouTube: a.YouTube, TikTok: a.TikTok, Twitter: a.Twitter,
		}
		if a.Tags != nil {
			t := nomads.NormalizeTags(*a.Tags)
			patch.Tags = &t
		}
		if patch.IsEmpty() {
			return "", nomads.Errorf(nomads.ErrInvalidInput,
				"no fields supplied; pass at least one of bio, tags, website, instagram, youtube, tiktok, twitter")
		}
		p, err := c.UpdateProfile(ctx, patch)
		if err != nil {
			return "", err
		}
		return jsonResult(map[string]any{"updated": true, "profile": p})

	case "nomads_list_trips":
		trips, err := c.ListTrips(ctx)
		if err != nil {
			return "", err
		}
		return jsonResult(map[string]any{"count": len(trips), "trips": trips})

	case "nomads_add_trip":
		var a tripArg
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", nomads.Wrap(err, nomads.ErrInvalidInput, "invalid arguments")
		}
		desired, err := toDesired([]tripArg{a})
		if err != nil {
			return "", err
		}
		d := desired[0]

		// Refuse to create a duplicate, so a retrying agent stays idempotent.
		current, err := c.ListTrips(ctx)
		if err != nil {
			return "", err
		}
		for _, t := range current {
			if t.Place() == d.Place() &&
				t.StartDate.Compare(d.StartDate) == 0 && t.EndDate.Compare(d.EndDate) == 0 {
				return "", nomads.Errorf(nomads.ErrTripAlreadyExists,
					"this trip already exists: %s", t.Label()).WithDetail("trip_id", t.ID)
			}
		}

		trip, err := c.CreateTrip(ctx, nomads.CreateTripRequest{
			City: d.City, Country: d.Country, StartDate: d.StartDate,
			EndDate: d.EndDate, Note: d.Note, Slug: d.Slug,
		})
		if err != nil {
			return "", err
		}
		if err := c.VerifyTrip(ctx, *trip); err != nil {
			return "", err
		}
		return jsonResult(map[string]any{"added": true, "verified": true, "trip": trip})

	case "nomads_update_trip":
		var a struct {
			ID      string  `json:"id"`
			City    *string `json:"city"`
			Country *string `json:"country"`
			From    *string `json:"from"`
			To      *string `json:"to"`
			Note    *string `json:"note"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", nomads.Wrap(err, nomads.ErrInvalidInput, "invalid arguments")
		}
		if strings.TrimSpace(a.ID) == "" {
			return "", nomads.Errorf(nomads.ErrInvalidInput, "id is required")
		}
		patch := nomads.TripPatch{City: a.City, Country: a.Country, Note: a.Note}
		if a.From != nil {
			d, err := nomads.ParseDate(*a.From)
			if err != nil {
				return "", nomads.Errorf(nomads.ErrInvalidInput, "from: %s", err)
			}
			patch.StartDate = &d
		}
		if a.To != nil {
			d, err := nomads.ParseDate(*a.To)
			if err != nil {
				return "", nomads.Errorf(nomads.ErrInvalidInput, "to: %s", err)
			}
			patch.EndDate = &d
		}
		if patch.IsEmpty() {
			return "", nomads.Errorf(nomads.ErrInvalidInput, "no changes supplied")
		}
		trip, err := c.UpdateTrip(ctx, a.ID, patch)
		if err != nil {
			return "", err
		}
		if err := c.VerifyTrip(ctx, *trip); err != nil {
			return "", err
		}
		return jsonResult(map[string]any{
			"updated": true, "verified": true, "trip": trip,
			"note": "Nomads.com assigned a new id to this trip; use the id in 'trip'.",
		})

	case "nomads_delete_trip":
		var a struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", nomads.Wrap(err, nomads.ErrInvalidInput, "invalid arguments")
		}
		if strings.TrimSpace(a.ID) == "" {
			return "", nomads.Errorf(nomads.ErrInvalidInput, "id is required")
		}
		if err := c.DeleteTrip(ctx, a.ID); err != nil {
			return "", err
		}
		return jsonResult(map[string]any{"deleted": true, "verified": true, "id": a.ID})

	case "nomads_plan_trip_sync":
		var a syncArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", nomads.Wrap(err, nomads.ErrInvalidInput, "invalid arguments")
		}
		desired, err := toDesired(a.Trips)
		if err != nil {
			return "", err
		}
		opts, err := syncOptions(a)
		if err != nil {
			return "", err
		}
		current, err := c.ListTrips(ctx)
		if err != nil {
			return "", err
		}
		// Planning is pure: nothing is written here.
		return jsonResult(summarise(nsync.PlanTripSync(current, desired, opts)))

	case "nomads_sync_trips":
		var a syncArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", nomads.Wrap(err, nomads.ErrInvalidInput, "invalid arguments")
		}
		desired, err := toDesired(a.Trips)
		if err != nil {
			return "", err
		}
		opts, err := syncOptions(a)
		if err != nil {
			return "", err
		}
		res, plan, syncErr := nsync.Sync(ctx, c, desired, opts)
		if res == nil {
			return "", syncErr
		}
		out := map[string]any{
			"created":   labels(res.Created),
			"updated":   labels(res.Updated),
			"deleted":   labels(res.Deleted),
			"unchanged": labels(res.Unchanged),
			"ambiguous": summarise(plan).Ambiguous,
			"verified":  syncErr == nil && !res.HasErrors(),
		}
		if len(res.Errors) > 0 {
			out["errors"] = res.Errors
		}
		if syncErr != nil {
			out["warning"] = syncErr.Error()
		}
		return jsonResult(out)
	}
	return "", nomads.Errorf(nomads.ErrInvalidInput, "unknown tool %q", name)
}

// labels renders trips as human-readable strings.
func labels(trips []nomads.Trip) []string {
	out := make([]string, 0, len(trips))
	for _, t := range trips {
		out = append(out, fmt.Sprintf("%s [%s]", t.Label(), t.ID))
	}
	return out
}
