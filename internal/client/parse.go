package client

import (
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// The profile page is server-rendered HTML with the authenticated values in the
// edit-modal inputs and one <tr class="trip"> per trip. Parsing is done with
// targeted regexes rather than a DOM library to keep dependencies minimal; each
// pattern is anchored on the stable class/data attributes documented in
// docs/api-observations.md.

var (
	// A trip row, captured whole so its data-* attributes can be read.
	tripRowRe = regexp.MustCompile(`(?is)<tr[^>]*\bclass="[^"]*\btrip\b[^"]*"[^>]*>.*?</tr>`)
	// data-* attribute lookup within one row.
	dataAttrRe = regexp.MustCompile(`(?is)\bdata-([a-z-]+)\s*=\s*"([^"]*)"`)
	// The city name lives in the .name cell's <h2>.
	cityRe = regexp.MustCompile(`(?is)<td[^>]*class="[^"]*\bname\b[^"]*"[^>]*>.*?<h2[^>]*>(.*?)</h2>`)
	// The country lives in the .country cell.
	countryRe = regexp.MustCompile(`(?is)<td[^>]*class="[^"]*\bcountry\b[^"]*"[^>]*>(.*?)</td>`)
	// A note row follows its trip row.
	tagStripRe = regexp.MustCompile(`(?is)<[^>]+>`)
)

// parseTrips extracts every trip from the profile HTML.
func parseTrips(page string) ([]nomads.Trip, error) {
	rows := tripRowRe.FindAllString(page, -1)
	trips := make([]nomads.Trip, 0, len(rows))

	for _, row := range rows {
		attrs := map[string]string{}
		for _, m := range dataAttrRe.FindAllStringSubmatch(row, -1) {
			attrs[strings.ToLower(m[1])] = html.UnescapeString(m[2])
		}
		id := attrs["trip-id"]
		if id == "" {
			// Not a real trip row (e.g. the editor template).
			continue
		}
		start, errS := nomads.ParseDate(attrs["date-start"])
		end, errE := nomads.ParseDate(attrs["date-end"])
		if errS != nil || errE != nil {
			// A row without parseable dates means the markup changed shape.
			continue
		}
		t := nomads.Trip{
			ID:        id,
			StartDate: start,
			EndDate:   end,
			Slug:      attrs["slug"],
			Latitude:  parseFloat(attrs["latitude"]),
			Longitude: parseFloat(attrs["longitude"]),
		}
		if m := cityRe.FindStringSubmatch(row); m != nil {
			t.City = cleanText(m[1])
		}
		if m := countryRe.FindStringSubmatch(row); m != nil {
			t.Country = cleanText(m[1])
		}
		// Fall back to the slug when the cells are missing.
		if t.City == "" && t.Slug != "" {
			t.City = slugCity(t.Slug)
		}
		trips = append(trips, t)
	}

	// The page renders newest-first; normalise to chronological order.
	nomads.SortTrips(trips)

	if len(rows) > 0 && len(trips) == 0 {
		return nil, nomads.Errorf(nomads.ErrAPIChanged,
			"found %d trip rows but none could be parsed; the profile markup may have changed",
			len(rows)).WithDetail("row_count", len(rows))
	}
	return trips, nil
}

// parseProfile extracts the editable profile fields.
func parseProfile(page string) *nomads.Profile {
	return &nomads.Profile{
		Bio:       inputValue(page, "edit-bio"),
		Website:   inputValue(page, "edit-website"),
		Twitter:   inputValue(page, "edit-twitter"),
		Instagram: inputValue(page, "edit-instagram"),
		YouTube:   inputValue(page, "edit-youtube"),
		TikTok:    inputValue(page, "edit-tiktok"),
		Tags:      parseTags(page),
	}
}

// inputValue reads the value of an <input class="..."> or the body of a
// <textarea class="...">, which is how the edit modal carries each field.
func inputValue(page, class string) string {
	// <textarea class="edit-bio" ...>value</textarea>
	ta := regexp.MustCompile(`(?is)<textarea[^>]*\bclass="[^"]*\b` +
		regexp.QuoteMeta(class) + `\b[^"]*"[^>]*>(.*?)</textarea>`)
	if m := ta.FindStringSubmatch(page); m != nil {
		return html.UnescapeString(strings.TrimSpace(m[1]))
	}
	// <input class="edit-website" value="..."> in either attribute order.
	in := regexp.MustCompile(`(?is)<input[^>]*\bclass="[^"]*\b` +
		regexp.QuoteMeta(class) + `\b[^"]*"[^>]*>`)
	m := in.FindString(page)
	if m == "" {
		return ""
	}
	val := regexp.MustCompile(`(?is)\bvalue\s*=\s*"([^"]*)"`).FindStringSubmatch(m)
	if val == nil {
		return ""
	}
	return html.UnescapeString(strings.TrimSpace(val[1]))
}

// tagSpanRe matches one selectable tag in the match-settings block.
var tagSpanRe = regexp.MustCompile(`(?is)<span[^>]*\bdata-category="([^"]*)"[^>]*\bdata-key="([^"]*)"[^>]*>`)

// parseTags returns the tags currently active on the profile.
//
// The remote key is "<category>_<key>", but users think in terms of the key
// alone ("Web Dev"), so that is what the domain model carries.
func parseTags(page string) []string {
	var out []string
	for _, m := range tagSpanRe.FindAllStringSubmatch(page, -1) {
		full := m[0]
		if !strings.Contains(full, "active") {
			continue
		}
		// Guard against "active" appearing in an unrelated attribute.
		if !regexp.MustCompile(`\bclass="[^"]*\bactive\b[^"]*"`).MatchString(full) {
			continue
		}
		key := html.UnescapeString(m[2])
		if key != "" {
			out = append(out, key)
		}
	}
	return nomads.NormalizeTags(out)
}

// cleanText strips tags and collapses the whitespace the template emits.
func cleanText(s string) string {
	s = tagStripRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// slugCity turns "sao-paulo-brazil" into a rough city name, used only when the
// table cells are unavailable.
func slugCity(slug string) string {
	parts := strings.Split(slug, "-")
	if len(parts) > 1 {
		parts = parts[:len(parts)-1] // drop the trailing country segment
	}
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}
