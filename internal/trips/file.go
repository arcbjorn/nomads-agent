// Package trips loads desired travel state from a file.
package trips

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// Load reads a desired-trips file, accepting the documented YAML shape or JSON.
func Load(path string) ([]nomads.DesiredTrip, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return parseJSON(b)
	default:
		// Try JSON anyway: a .yaml file containing JSON is still valid YAML.
		if trimmed := strings.TrimSpace(string(b)); strings.HasPrefix(trimmed, "{") {
			return parseJSON(b)
		}
		return ParseYAML(string(b))
	}
}

func parseJSON(b []byte) ([]nomads.DesiredTrip, error) {
	var doc struct {
		Trips []nomads.DesiredTrip `json:"trips"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, nomads.Wrap(err, nomads.ErrInvalidInput, "parse trips JSON")
	}
	return doc.Trips, nil
}

// ParseYAML reads the small YAML subset this tool documents:
//
//	trips:
//	  - city: Lisbon
//	    country: Portugal
//	    from: 2030-04-10
//	    to: 2030-04-18
//
// A full YAML parser is a heavy dependency for one fixed shape, so this handles
// exactly that structure and reports anything else as an error rather than
// guessing.
func ParseYAML(src string) ([]nomads.DesiredTrip, error) {
	var (
		out     []nomads.DesiredTrip
		cur     *nomads.DesiredTrip
		inTrips bool
	)
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}

	for n, raw := range strings.Split(src, "\n") {
		line := stripComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		trimmed := strings.TrimSpace(line)

		// Top-level "trips:" key.
		if indent == 0 {
			if strings.HasPrefix(trimmed, "trips:") {
				flush()
				inTrips = true
				continue
			}
			flush()
			inTrips = false
			continue
		}
		if !inTrips {
			continue
		}

		// A new list item starts a new trip.
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			flush()
			cur = &nomads.DesiredTrip{}
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			if trimmed == "" {
				continue
			}
		}
		if cur == nil {
			continue
		}

		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			return nil, nomads.Errorf(nomads.ErrInvalidInput,
				"line %d: expected 'key: value', got %q", n+1, trimmed)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = unquote(strings.TrimSpace(value))

		switch key {
		case "city":
			cur.City = value
		case "country":
			cur.Country = value
		case "from", "start", "start_date":
			dt, err := nomads.ParseDate(value)
			if err != nil {
				return nil, nomads.Errorf(nomads.ErrInvalidInput, "line %d: %s", n+1, err)
			}
			cur.StartDate = dt
		case "to", "end", "end_date":
			dt, err := nomads.ParseDate(value)
			if err != nil {
				return nil, nomads.Errorf(nomads.ErrInvalidInput, "line %d: %s", n+1, err)
			}
			cur.EndDate = dt
		case "note":
			cur.Note = value
		case "slug", "city_slug":
			cur.Slug = value
		default:
			return nil, nomads.Errorf(nomads.ErrInvalidInput,
				"line %d: unknown field %q (expected city, country, from, to, note)", n+1, key)
		}
	}
	flush()

	if len(out) == 0 {
		return nil, nomads.Errorf(nomads.ErrInvalidInput,
			"no trips found; expected a top-level 'trips:' list")
	}
	return out, nil
}

// stripComment removes a trailing # comment that is not inside quotes.
func stripComment(line string) string {
	var inSingle, inDouble bool
	for i, r := range line {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return line[:i]
			}
		}
	}
	return line
}

// unquote removes matching surrounding quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
