package stats

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// minuteAt returns t0 plus i minutes.
func minuteAt(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }

// feedMinute records 12 samples 5 s apart inside minute, then one more call at the
// first second of the next minute so that the minute is committed. That last call
// (k == 12) belongs to the following minute and is the one that triggers the commit.
func feedMinute(t *testing.T, s *Store, item string, minute time.Time, perSample func(k int) Sample) {
	t.Helper()
	for k := 0; k <= 12; k++ {
		if err := s.Record(item, minute.Add(time.Duration(k)*5*time.Second), perSample(k)); err != nil {
			t.Fatalf("record %d: %v", k, err)
		}
	}
}

func f(v float64) *float64 { return &v }
