package nomads

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"time"
)

// DateLayout is the wire and storage format for all travel dates.
const DateLayout = "2006-01-02"

// Date is a timezone-free calendar date.
//
// Travel dates are calendar facts ("I land on the 25th"), not instants. Storing
// them as time.Time invites a timezone shift to silently move a trip a day when
// marshalled, compared, or sent to an API in another zone. Date carries only
// year/month/day so that class of bug cannot occur.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// NewDate builds a Date from its parts, normalising out-of-range values the way
// time.Date does (e.g. Jan 32 becomes Feb 1).
func NewDate(year int, month time.Month, day int) Date {
	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return Date{t.Year(), t.Month(), t.Day()}
}

// ParseDate parses an ISO "YYYY-MM-DD" date.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(DateLayout, strings.TrimSpace(s))
	if err != nil {
		return Date{}, fmt.Errorf("invalid date %q, want YYYY-MM-DD", s)
	}
	return Date{t.Year(), t.Month(), t.Day()}, nil
}

// MustParseDate is ParseDate for tests and constants.
func MustParseDate(s string) Date {
	d, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

// DateFromTime takes the calendar date of t in its own location.
func DateFromTime(t time.Time) Date {
	return Date{t.Year(), t.Month(), t.Day()}
}

// IsZero reports whether the date is unset.
func (d Date) IsZero() bool { return d == Date{} }

// String renders ISO "YYYY-MM-DD".
func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day)
}

// Time materialises the date at midnight UTC. Use only at an API boundary that
// demands an instant; never for comparison or storage.
func (d Date) Time() time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// Unix is the epoch seconds of midnight UTC on this date.
func (d Date) Unix() int64 { return d.Time().Unix() }

// Compare orders two dates: -1 before, 0 equal, +1 after.
func (d Date) Compare(o Date) int {
	switch {
	case d.Year != o.Year:
		return sign(d.Year - o.Year)
	case d.Month != o.Month:
		return sign(int(d.Month) - int(o.Month))
	case d.Day != o.Day:
		return sign(d.Day - o.Day)
	}
	return 0
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}

// Before reports whether d is strictly earlier than o.
func (d Date) Before(o Date) bool { return d.Compare(o) < 0 }

// After reports whether d is strictly later than o.
func (d Date) After(o Date) bool { return d.Compare(o) > 0 }

// AddDays returns the date n days later (n may be negative).
func (d Date) AddDays(n int) Date { return DateFromTime(d.Time().AddDate(0, 0, n)) }

// DaysUntil is the number of days from d to o, negative if o precedes d.
func (d Date) DaysUntil(o Date) int {
	return int(o.Time().Sub(d.Time()).Hours() / 24)
}

// MarshalJSON renders the date as an ISO string.
func (d Date) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.String() + `"`), nil
}

// UnmarshalJSON accepts an ISO string, treating "" and null as the zero date.
func (d *Date) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*d = Date{}
		return nil
	}
	parsed, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// MarshalYAML renders the date as an ISO string.
func (d Date) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML accepts an ISO string or a YAML-native date.
func (d *Date) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	if strings.TrimSpace(s) == "" {
		*d = Date{}
		return nil
	}
	parsed, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// Value implements driver.Valuer, storing the ISO string.
func (d Date) Value() (driver.Value, error) { return d.String(), nil }

// Scan implements sql.Scanner for ISO strings and time values.
func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = Date{}
		return nil
	case time.Time:
		*d = DateFromTime(v)
		return nil
	case []byte:
		return d.scanString(string(v))
	case string:
		return d.scanString(v)
	}
	return fmt.Errorf("cannot scan %T into Date", src)
}

func (d *Date) scanString(s string) error {
	if strings.TrimSpace(s) == "" {
		*d = Date{}
		return nil
	}
	parsed, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}
