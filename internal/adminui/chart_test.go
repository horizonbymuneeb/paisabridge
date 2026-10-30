package adminui

import (
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// TestChart14BucketsByDay: orders land in the right day, out-of-window rows are
// ignored, and heights are relative to the busiest day.
func TestChart14BucketsByDay(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	mk := func(daysAgo int) *types.Order {
		return &types.Order{CreatedAt: now.AddDate(0, 0, -daysAgo)}
	}
	orders := []*types.Order{
		mk(0), mk(0), mk(0), // today: 3
		mk(1),                    // yesterday: 1
		mk(13),                   // first day of the window: 1
		mk(14),                   // outside: ignored
		{CreatedAt: time.Time{}}, // zero: ignored
	}
	bars, max, total := chart14(orders, now)
	if len(bars) != 14 {
		t.Fatalf("got %d bars, want 14", len(bars))
	}
	if max != 3 {
		t.Fatalf("max = %d, want 3", max)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5 (14-day and zero rows excluded)", total)
	}
	if bars[13].Count != 3 || bars[13].Height != 100 {
		t.Fatalf("today = %+v, want count 3 at full height", bars[13])
	}
	if bars[12].Count != 1 || bars[12].Height != 33 {
		t.Fatalf("yesterday = %+v, want count 1 at 33%%", bars[12])
	}
	if bars[0].Count != 1 {
		t.Fatalf("oldest in-window day = %+v, want count 1", bars[0])
	}
}

// TestChart14Empty: an empty window must not divide by zero.
func TestChart14Empty(t *testing.T) {
	bars, max, total := chart14(nil, time.Now())
	if max != 0 || total != 0 || len(bars) != 14 {
		t.Fatalf("empty chart = (%d bars, max %d, total %d)", len(bars), max, total)
	}
	for _, b := range bars {
		if b.Height != 0 {
			t.Fatalf("empty bar has height %d", b.Height)
		}
	}
}
