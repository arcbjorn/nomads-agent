// Package nomads holds the normalised domain model for a Nomads.com account.
//
// Nothing in this package talks to the network or knows the shape of the
// Nomads.com wire protocol. Remote quirks are absorbed by internal/client so
// that the rest of the program depends only on these types.
package nomads

import (
	"fmt"
	"sort"
	"strings"
)

// Profile is the subset of a Nomads.com profile this tool can read and write.
type Profile struct {
	Username  string   `json:"username"`
	Bio       string   `json:"bio"`
	Tags      []string `json:"tags"`
	Website   string   `json:"website"`
	Twitter   string   `json:"twitter"`
	Instagram string   `json:"instagram"`
	YouTube   string   `json:"youtube"`
	TikTok    string   `json:"tiktok"`
}

// ProfilePatch is a sparse profile update: a nil field means "leave unchanged".
type ProfilePatch struct {
	Bio       *string   `json:"bio,omitempty"`
	Tags      *[]string `json:"tags,omitempty"`
	Website   *string   `json:"website,omitempty"`
	Twitter   *string   `json:"twitter,omitempty"`
	Instagram *string   `json:"instagram,omitempty"`
	YouTube   *string   `json:"youtube,omitempty"`
	TikTok    *string   `json:"tiktok,omitempty"`
}

// IsEmpty reports whether the patch would change nothing.
func (p ProfilePatch) IsEmpty() bool {
	return p.Bio == nil && p.Tags == nil && p.Website == nil &&
		p.Twitter == nil && p.Instagram == nil && p.YouTube == nil && p.TikTok == nil
}

// TripStatus is a trip's position relative to today.
type TripStatus string

const (
	TripPlanned   TripStatus = "planned"
	TripCurrent   TripStatus = "current"
	TripCompleted TripStatus = "completed"
)

// Trip is a stay in one city, as Nomads.com stores it.
type Trip struct {
	ID        string  `json:"id"`
	City      string  `json:"city"`
	Country   string  `json:"country"`
	Slug      string  `json:"slug,omitempty"`
	StartDate Date    `json:"start_date"`
	EndDate   Date    `json:"end_date"`
	Latitude  float64 `json:"latitude,omitempty"`
	Longitude float64 `json:"longitude,omitempty"`
	Note      string  `json:"note,omitempty"`
}

// StatusOn classifies the trip relative to the given day.
func (t Trip) StatusOn(today Date) TripStatus {
	switch {
	case t.EndDate.Before(today):
		return TripCompleted
	case t.StartDate.After(today):
		return TripPlanned
	default:
		return TripCurrent
	}
}

// Label is a short human description used in plans and errors.
func (t Trip) Label() string {
	return fmt.Sprintf("%s, %s (%s -> %s)", t.City, t.Country, t.StartDate, t.EndDate)
}

// Place is the canonical identity of a trip's location, used for matching.
func (t Trip) Place() string { return CanonicalPlace(t.City, t.Country) }

// Overlaps reports whether two trips share at least one day.
func (t Trip) Overlaps(o Trip) bool {
	return !t.StartDate.After(o.EndDate) && !o.StartDate.After(t.EndDate)
}

// DesiredTrip is one entry of the caller's intended travel state.
type DesiredTrip struct {
	City      string `json:"city" yaml:"city"`
	Country   string `json:"country" yaml:"country"`
	StartDate Date   `json:"from" yaml:"from"`
	EndDate   Date   `json:"to" yaml:"to"`
	Note      string `json:"note,omitempty" yaml:"note,omitempty"`
	// Slug optionally pins the Nomads.com city, e.g. "lisbon-portugal". When
	// set, the official API uses it directly instead of geocoding the name.
	Slug string `json:"slug,omitempty" yaml:"slug,omitempty"`
}

// Place is the canonical identity of the desired location.
func (d DesiredTrip) Place() string { return CanonicalPlace(d.City, d.Country) }

// Label is a short human description used in plans and errors.
func (d DesiredTrip) Label() string {
	return fmt.Sprintf("%s, %s (%s -> %s)", d.City, d.Country, d.StartDate, d.EndDate)
}

// Validate checks a desired trip is internally coherent.
func (d DesiredTrip) Validate() error {
	if strings.TrimSpace(d.City) == "" {
		return fmt.Errorf("city is required")
	}
	if d.StartDate.IsZero() {
		return fmt.Errorf("%s: start date is required", d.City)
	}
	if d.EndDate.IsZero() {
		return fmt.Errorf("%s: end date is required", d.City)
	}
	if d.EndDate.Before(d.StartDate) {
		return fmt.Errorf("%s: end date %s is before start date %s", d.City, d.EndDate, d.StartDate)
	}
	return nil
}

// Overlaps reports whether two desired trips share at least one day.
func (d DesiredTrip) Overlaps(o DesiredTrip) bool {
	return !d.StartDate.After(o.EndDate) && !o.StartDate.After(d.EndDate)
}

// CreateTripRequest asks the remote to create a trip.
type CreateTripRequest struct {
	City      string  `json:"city"`
	Country   string  `json:"country"`
	StartDate Date    `json:"start_date"`
	EndDate   Date    `json:"end_date"`
	Note      string  `json:"note,omitempty"`
	Latitude  float64 `json:"latitude,omitempty"`
	Longitude float64 `json:"longitude,omitempty"`
	// Slug is the Nomads.com city slug, e.g. "lisbon-portugal". When set, the
	// official API uses it directly instead of geocoding coordinates.
	Slug string `json:"slug,omitempty"`
}

// Label is a short human description used in plans and errors.
func (c CreateTripRequest) Label() string {
	return fmt.Sprintf("%s, %s (%s -> %s)", c.City, c.Country, c.StartDate, c.EndDate)
}

// TripPatch is a sparse trip update: a nil field means "leave unchanged".
type TripPatch struct {
	City      *string  `json:"city,omitempty"`
	Country   *string  `json:"country,omitempty"`
	StartDate *Date    `json:"start_date,omitempty"`
	EndDate   *Date    `json:"end_date,omitempty"`
	Note      *string  `json:"note,omitempty"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
}

// IsEmpty reports whether the patch would change nothing.
func (p TripPatch) IsEmpty() bool {
	return p.City == nil && p.Country == nil && p.StartDate == nil &&
		p.EndDate == nil && p.Note == nil && p.Latitude == nil && p.Longitude == nil
}

// CanonicalPlace normalises a city/country pair into a comparison key, so that
// "Kraków, Poland" and "krakow,  poland" identify the same place.
func CanonicalPlace(city, country string) string {
	c := CanonicalName(city)
	if country == "" {
		return c
	}
	return c + "|" + CanonicalName(country)
}

// CanonicalName folds a place name for comparison: case, accents, punctuation
// and internal whitespace are all normalised away.
func CanonicalName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		r = foldRune(r)
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevSpace = false
		case r == ' ' || r == '-' || r == '_':
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// foldRune maps the accented Latin letters common in city names to ASCII.
func foldRune(r rune) rune {
	switch r {
	case 'á', 'à', 'â', 'ã', 'ä', 'å', 'ā', 'ă', 'ą':
		return 'a'
	case 'é', 'è', 'ê', 'ë', 'ē', 'ĕ', 'ė', 'ę', 'ě':
		return 'e'
	case 'í', 'ì', 'î', 'ï', 'ī', 'į':
		return 'i'
	case 'ó', 'ò', 'ô', 'õ', 'ö', 'ø', 'ō':
		return 'o'
	case 'ú', 'ù', 'û', 'ü', 'ū', 'ů':
		return 'u'
	case 'ç', 'ć', 'č':
		return 'c'
	case 'ñ', 'ń', 'ň':
		return 'n'
	case 'ş', 'š', 'ś':
		return 's'
	case 'ž', 'ź', 'ż':
		return 'z'
	case 'ý', 'ÿ':
		return 'y'
	case 'ğ':
		return 'g'
	case 'ł':
		return 'l'
	case 'ř':
		return 'r'
	case 'ť':
		return 't'
	case 'đ', 'ď':
		return 'd'
	case 'ß':
		return 's'
	}
	return r
}

// SortTrips orders trips by start date, then end date, then place, so output is
// deterministic regardless of the order the remote returned them in.
func SortTrips(trips []Trip) {
	sort.SliceStable(trips, func(i, j int) bool {
		if c := trips[i].StartDate.Compare(trips[j].StartDate); c != 0 {
			return c < 0
		}
		if c := trips[i].EndDate.Compare(trips[j].EndDate); c != 0 {
			return c < 0
		}
		return trips[i].Place() < trips[j].Place()
	})
}

// NormalizeTags trims, drops blanks, and removes case-insensitive duplicates
// while preserving the caller's ordering.
func NormalizeTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	return out
}
