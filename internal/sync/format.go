package sync

import (
	"fmt"
	"strings"
)

// Format renders a plan as the human-readable dry-run report.
func Format(p SyncPlan) string {
	var b strings.Builder
	b.WriteString("Nomads sync plan\n")

	section(&b, "CREATE", len(p.Creates), func() {
		for _, c := range p.Creates {
			fmt.Fprintf(&b, "  %s, %s\n", c.City, c.Country)
			fmt.Fprintf(&b, "  %s -> %s\n", c.StartDate, c.EndDate)
		}
	})

	section(&b, "UPDATE", len(p.Updates), func() {
		for _, u := range p.Updates {
			fmt.Fprintf(&b, "  %s, %s\n", u.Current.City, u.Current.Country)
			fmt.Fprintf(&b, "  %s -> %s\n", u.Current.StartDate, u.Current.EndDate)
			fmt.Fprintf(&b, "  to:\n")
			fmt.Fprintf(&b, "  %s -> %s\n", u.Desired.StartDate, u.Desired.EndDate)
			if len(u.Changes) > 0 {
				fmt.Fprintf(&b, "  (%s)\n", strings.Join(u.Changes, ", "))
			}
		}
	})

	section(&b, "UNCHANGED", len(p.Unchanged), func() {
		for _, u := range p.Unchanged {
			fmt.Fprintf(&b, "  %s, %s\n", u.Current.City, u.Current.Country)
		}
	})

	section(&b, "DELETE", len(p.Deletes), func() {
		for _, d := range p.Deletes {
			fmt.Fprintf(&b, "  %s, %s\n", d.City, d.Country)
			fmt.Fprintf(&b, "  %s -> %s\n", d.StartDate, d.EndDate)
		}
	})

	section(&b, "AMBIGUOUS", len(p.Ambiguous), func() {
		for _, a := range p.Ambiguous {
			fmt.Fprintf(&b, "  %s\n", a.Desired.Label())
			for _, c := range a.Candidates {
				fmt.Fprintf(&b, "    candidate: %s\n", c.Label())
			}
			fmt.Fprintf(&b, "    %s\n", a.Reason)
		}
	})

	if len(p.Warnings) > 0 {
		b.WriteString("\nWARNINGS\n")
		for _, w := range p.Warnings {
			fmt.Fprintf(&b, "  %s\n", w)
		}
	}
	return b.String()
}

// section writes a titled block, or "none" when the block is empty.
func section(b *strings.Builder, title string, n int, body func()) {
	fmt.Fprintf(b, "\n%s\n", title)
	if n == 0 {
		b.WriteString("  none\n")
		return
	}
	body()
}
