package store

import (
	"fmt"
	"time"
)

// TimeLayout is how this package WRITES timestamps: fixed-width nanoseconds, so
// rows sort lexicographically and two writers agree on the byte format.
const TimeLayout = "2006-01-02T15:04:05.000000000Z"

// timeLayouts are the shapes parseTime ACCEPTS. Writing one format and reading
// only that format looks tidy but is a trap: a row written by a migration, a
// hand-run UPDATE, an older build, or an export/import tool carries fewer
// fractional digits, and a strict parse turns it into the zero time. Every
// caller then silently computes nonsense - an order with a zero expiry looks a
// thousand years overdue, and the UI prints "waiting 106751 d".
var timeLayouts = []string{
	TimeLayout,       // 9 digits, what we write
	time.RFC3339Nano, // variable digits
	time.RFC3339,     // no digits
	"2006-01-02T15:04:05Z",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseTime reads a stored timestamp in any accepted shape. An empty string is
// the zero time (columns default to ” before they are first written); anything
// else that cannot be parsed is an error rather than a silent zero, because a
// silently zeroed timestamp is how money logic goes wrong.
func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("store: unparseable timestamp %q", v)
}

// mustParseTime is parseTime for callers that already validated the row (tests,
// migrations): an unparseable value becomes the zero time so a caller can still
// inspect the row instead of failing the whole scan.
func mustParseTime(v string) time.Time {
	t, _ := parseTime(v)
	return t
}
