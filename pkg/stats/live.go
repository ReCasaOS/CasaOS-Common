package stats

import (
	"math"
	"sync"
	"time"
)

const liveStep = 5 // seconds per live slot

// liveRing keeps the last 15 minutes of a single item at 5 s. Values stay
// float64 (no rounding), NaN means "no sample".
type liveRing struct {
	mu     sync.RWMutex
	stamp  [LiveSlots]int64 // slot number held by each ring index; 0 = empty
	series map[string]*[LiveSlots]float64
}

func newLiveRing() *liveRing {
	return &liveRing{series: map[string]*[LiveSlots]float64{}}
}

func nanColumn() *[LiveSlots]float64 {
	a := new([LiveSlots]float64)
	for i := range a {
		a[i] = math.NaN()
	}
	return a
}

// put stores smp in the slot of at; the last put of a slot replaces it whole.
// A pre-1970 time (clock glitch) is ignored: its slot would be negative.
// ponytail: series are never evicted, only reset drops them; bounded by MaxSeries.
func (l *liveRing) put(at time.Time, smp Sample) {
	if at.Unix() < 0 {
		return
	}
	slot := at.Unix() / liveStep
	idx := int(slot % LiveSlots)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, col := range l.series {
		col[idx] = math.NaN()
	}
	l.stamp[idx] = slot
	for k, v := range smp {
		col, ok := l.series[k]
		if !ok {
			if len(l.series) >= MaxSeries {
				continue
			}
			col = nanColumn()
			l.series[k] = col
		}
		col[idx] = v
	}
}

// snapshot returns the 180-slot grid ending at floor(now/5)*5.
func (l *liveRing) snapshot(now time.Time) (from, to int64, series map[string][]float64) {
	last := now.Unix() / liveStep
	first := last - (LiveSlots - 1)
	l.mu.RLock()
	defer l.mu.RUnlock()
	series = make(map[string][]float64, len(l.series))
	for k, col := range l.series {
		out := make([]float64, LiveSlots)
		for i := range out {
			s := first + int64(i)
			idx := int(((s % LiveSlots) + LiveSlots) % LiveSlots) // s < 0 while the clock is under 895 s
			if l.stamp[idx] == s {
				out[i] = col[idx]
			} else {
				out[i] = math.NaN()
			}
		}
		series[k] = out
	}
	return first * liveStep, last * liveStep, series
}

func (l *liveRing) reset() {
	l.mu.Lock()
	l.stamp = [LiveSlots]int64{}
	l.series = map[string]*[LiveSlots]float64{}
	l.mu.Unlock()
}
