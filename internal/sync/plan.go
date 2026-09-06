// Package sync computes the difference between a desired travel state and the
// state currently stored on Nomads.com.
//
// Everything here is pure: PlanTripSync performs no network I/O, reads no
// clock, and is fully deterministic for a given input. That makes the risky
// part of this tool — deciding what to create, change and delete — exhaustively
// unit-testable without an account.
package sync

import (
	"fmt"
	"sort"
	"strings"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// MatchKind records why a desired trip was paired with a remote trip. It is
// reported so a human or agent can judge how much to trust the pairing.
type MatchKind string

const (
	// MatchExact means place and both dates already agree.
	MatchExact MatchKind = "exact"
	// MatchPlaceAndStart means place and start date agree; the end date differs.
	MatchPlaceAndStart MatchKind = "place_and_start_date"
	// MatchPlaceAndOverlap means the place agrees and the date ranges overlap.
	MatchPlaceAndOverlap MatchKind = "place_and_overlapping_dates"
)

// TripUpdate is a matched pair that needs the remote trip changed.
type TripUpdate struct {
	Current nomads.Trip        `json:"current"`
	Desired nomads.DesiredTrip `json:"desired"`
	Patch   nomads.TripPatch   `json:"-"`
	Match   MatchKind          `json:"match"`
	// Changes names the fields that differ, for display.
	Changes []string `json:"changes"`
}

// UnchangedTrip is a desired trip already satisfied by the remote state.
type UnchangedTrip struct {
	Current nomads.Trip        `json:"current"`
	Desired nomads.DesiredTrip `json:"desired"`
}

// AmbiguousMatch is a desired trip that could correspond to more than one
// remote trip. These are never mutated automatically.
type AmbiguousMatch struct {
	Desired    nomads.DesiredTrip `json:"desired"`
	Candidates []nomads.Trip      `json:"candidates"`
	Reason     string             `json:"reason"`
}

// SyncPlan is the full set of actions needed to reach the desired state.
type SyncPlan struct {
	Creates   []nomads.CreateTripRequest `json:"creates"`
	Updates   []TripUpdate               `json:"updates"`
	Deletes   []nomads.Trip              `json:"deletes"`
	Unchanged []UnchangedTrip            `json:"unchanged"`
	Ambiguous []AmbiguousMatch           `json:"ambiguous"`
	// Warnings flag non-fatal problems, e.g. overlapping desired trips.
	Warnings []string `json:"warnings,omitempty"`
}

// IsNoop reports whether applying the plan would change nothing remotely.
func (p SyncPlan) IsNoop() bool {
	return len(p.Creates) == 0 && len(p.Updates) == 0 && len(p.Deletes) == 0
}

// MutationCount is the number of remote writes the plan implies.
func (p SyncPlan) MutationCount() int {
	return len(p.Creates) + len(p.Updates) + len(p.Deletes)
}

// Options tunes planning.
type Options struct {
	// DeleteMissing allows removal of remote trips that the desired state does
	// not mention. Off by default: deletion is destructive and the desired list
	// is usually a partial view of a travel history.
	DeleteMissing bool
	// DeleteHorizon, when set, restricts deletions to trips starting on or after
	// this date. It protects historical travel from a forward-looking plan.
	DeleteHorizon *nomads.Date
}

// PlanTripSync computes the actions that reconcile current with desired.
//
// It performs no I/O and is deterministic. The matching strategy is deliberately
// conservative: anything it cannot pair confidently becomes an AmbiguousMatch
// for a human to resolve rather than a silent mutation.
func PlanTripSync(current []nomads.Trip, desired []nomads.DesiredTrip, opts Options) SyncPlan {
	plan := SyncPlan{}

	// Work on copies so the caller's slices are never reordered.
	remote := append([]nomads.Trip(nil), current...)
	nomads.SortTrips(remote)

	want := append([]nomads.DesiredTrip(nil), desired...)
	sortDesired(want)

	plan.Warnings = append(plan.Warnings, overlapWarnings(want)...)

	// claimed guards against two desired trips matching the same remote trip.
	claimed := make(map[int]bool, len(remote))
	matchedDesired := make(map[int]bool, len(want))

	// Three passes, strongest signal first. A weaker pass may only consider
	// remote trips that no stronger pass has already claimed, so a desired trip
	// can never steal a remote trip that matched something else exactly.
	for _, kind := range []MatchKind{MatchExact, MatchPlaceAndStart, MatchPlaceAndOverlap} {
		for di, d := range want {
			if matchedDesired[di] {
				continue
			}
			candidates := candidatesFor(d, remote, claimed, kind)
			switch len(candidates) {
			case 0:
				continue
			case 1:
				ri := candidates[0]
				claimed[ri] = true
				matchedDesired[di] = true
				recordPair(&plan, remote[ri], d, kind)
			default:
				// Several equally plausible remote trips: refuse to guess.
				matchedDesired[di] = true
				cands := make([]nomads.Trip, 0, len(candidates))
				for _, ri := range candidates {
					cands = append(cands, remote[ri])
				}
				plan.Ambiguous = append(plan.Ambiguous, AmbiguousMatch{
					Desired:    d,
					Candidates: cands,
					Reason: fmt.Sprintf("%d remote trips match %s by %s; refusing to guess",
						len(cands), d.Label(), kind),
				})
			}
		}
	}

	// Desired trips with no remote counterpart become creations.
	for di, d := range want {
		if matchedDesired[di] {
			continue
		}
		plan.Creates = append(plan.Creates, nomads.CreateTripRequest{
			City:      strings.TrimSpace(d.City),
			Country:   strings.TrimSpace(d.Country),
			StartDate: d.StartDate,
			EndDate:   d.EndDate,
			Note:      d.Note,
			Slug:      strings.TrimSpace(d.Slug),
		})
	}

	// Unclaimed remote trips are only deleted when explicitly requested.
	if opts.DeleteMissing {
		for ri, t := range remote {
			if claimed[ri] {
				continue
			}
			if opts.DeleteHorizon != nil && t.StartDate.Before(*opts.DeleteHorizon) {
				continue
			}
			plan.Deletes = append(plan.Deletes, t)
		}
	}

	return plan
}

// candidatesFor returns the indices of unclaimed remote trips matching d at the
// given strength.
func candidatesFor(d nomads.DesiredTrip, remote []nomads.Trip, claimed map[int]bool, kind MatchKind) []int {
	var out []int
	for ri, t := range remote {
		if claimed[ri] || !placeMatches(d, t) {
			continue
		}
		var ok bool
		switch kind {
		case MatchExact:
			ok = t.StartDate.Compare(d.StartDate) == 0 && t.EndDate.Compare(d.EndDate) == 0
		case MatchPlaceAndStart:
			ok = t.StartDate.Compare(d.StartDate) == 0
		case MatchPlaceAndOverlap:
			ok = !t.StartDate.After(d.EndDate) && !d.StartDate.After(t.EndDate)
		}
		if ok {
			out = append(out, ri)
		}
	}
	return out
}

// placeMatches compares canonicalised city/country. A desired trip that omits
// the country matches on city alone, since the country is then unconstrained.
func placeMatches(d nomads.DesiredTrip, t nomads.Trip) bool {
	if nomads.CanonicalName(d.City) != nomads.CanonicalName(t.City) {
		return false
	}
	if strings.TrimSpace(d.Country) == "" {
		return true
	}
	return nomads.CanonicalName(d.Country) == nomads.CanonicalName(t.Country)
}

// recordPair files a matched pair as either unchanged or an update.
func recordPair(plan *SyncPlan, t nomads.Trip, d nomads.DesiredTrip, kind MatchKind) {
	patch := nomads.TripPatch{}
	var changes []string

	if t.StartDate.Compare(d.StartDate) != 0 {
		v := d.StartDate
		patch.StartDate = &v
		changes = append(changes, fmt.Sprintf("start %s -> %s", t.StartDate, d.StartDate))
	}
	if t.EndDate.Compare(d.EndDate) != 0 {
		v := d.EndDate
		patch.EndDate = &v
		changes = append(changes, fmt.Sprintf("end %s -> %s", t.EndDate, d.EndDate))
	}
	// Only correct the country when the caller actually specified one.
	if c := strings.TrimSpace(d.Country); c != "" &&
		nomads.CanonicalName(c) != nomads.CanonicalName(t.Country) {
		patch.Country = &c
		changes = append(changes, fmt.Sprintf("country %s -> %s", t.Country, c))
	}
	if d.Note != "" && d.Note != t.Note {
		v := d.Note
		patch.Note = &v
		changes = append(changes, "note")
	}

	if patch.IsEmpty() {
		plan.Unchanged = append(plan.Unchanged, UnchangedTrip{Current: t, Desired: d})
		return
	}
	plan.Updates = append(plan.Updates, TripUpdate{
		Current: t, Desired: d, Patch: patch, Match: kind, Changes: changes,
	})
}

// overlapWarnings reports desired trips that claim the same days. Nomads.com
// allows this (and offers to split), but it is usually a mistake worth naming.
func overlapWarnings(want []nomads.DesiredTrip) []string {
	var out []string
	for i := 0; i < len(want); i++ {
		for j := i + 1; j < len(want); j++ {
			if !want[i].Overlaps(want[j]) {
				continue
			}
			// Back-to-back trips sharing only the travel day are normal.
			if want[i].EndDate.Compare(want[j].StartDate) == 0 ||
				want[j].EndDate.Compare(want[i].StartDate) == 0 {
				continue
			}
			out = append(out, fmt.Sprintf("desired trips overlap: %s and %s",
				want[i].Label(), want[j].Label()))
		}
	}
	return out
}

// sortDesired orders desired trips deterministically by date then place.
func sortDesired(d []nomads.DesiredTrip) {
	sort.SliceStable(d, func(i, j int) bool {
		if c := d[i].StartDate.Compare(d[j].StartDate); c != 0 {
			return c < 0
		}
		if c := d[i].EndDate.Compare(d[j].EndDate); c != 0 {
			return c < 0
		}
		return d[i].Place() < d[j].Place()
	})
}

// ValidateDesired checks every desired trip and reports the first problem.
func ValidateDesired(desired []nomads.DesiredTrip) error {
	for _, d := range desired {
		if err := d.Validate(); err != nil {
			return nomads.Wrap(err, nomads.ErrInvalidInput, "%s", err.Error())
		}
	}
	return nil
}
