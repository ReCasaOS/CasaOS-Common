package stats

import (
	"fmt"
	"math"
	"testing"
	"time"
)

var liveT0 = time.Unix(1_700_000_000, 0) // multiple of 5

func TestLiveGridShape(t *testing.T) {
	l := newLiveRing()
	l.put(liveT0, Sample{"cpu/user": 1})
	from, to, s := l.snapshot(liveT0.Add(2 * time.Second))
	if to-from != 179*5 || to%5 != 0 || len(s["cpu/user"]) != 180 {
		t.Fatalf("from=%d to=%d len=%d", from, to, len(s["cpu/user"]))
	}
	if v := s["cpu/user"][179]; v != 1 {
		t.Fatalf("last=%v", v)
	}
}

func TestLiveExactValues(t *testing.T) {
	l := newLiveRing()
	a, b := 0.1, 0.2
	l.put(liveT0.Add(5*time.Second), Sample{"cpu/user": a + b})
	_, _, s := l.snapshot(liveT0.Add(5 * time.Second))
	if s["cpu/user"][179] != 0.30000000000000004 {
		t.Fatalf("got %v", s["cpu/user"][179])
	}
}

func TestLiveMissingIsNaN(t *testing.T) {
	l := newLiveRing()
	for _, k := range []int{0, 1, 3} {
		l.put(liveT0.Add(time.Duration(k*5)*time.Second), Sample{"a/b": float64(k + 1)})
	}
	_, _, s := l.snapshot(liveT0.Add(15 * time.Second))
	v := s["a/b"]
	if v[176] != 1 || v[177] != 2 || !math.IsNaN(v[178]) || v[179] != 4 {
		t.Fatalf("tail=%v", v[176:])
	}
}

func TestLiveWindowSlides(t *testing.T) {
	l := newLiveRing()
	var last time.Time
	for i := 0; i <= 240; i++ { // 20 minutes
		last = liveT0.Add(time.Duration(i*5) * time.Second)
		l.put(last, Sample{"a/b": float64(i)})
	}
	_, _, s := l.snapshot(last)
	v := s["a/b"]
	if len(v) != 180 || v[0] != 240-179 || v[179] != 240 {
		t.Fatalf("len=%d first=%v last=%v", len(v), v[0], v[179])
	}
}

func TestLiveSameSlotLastWins(t *testing.T) {
	l := newLiveRing()
	l.put(liveT0.Add(time.Second), Sample{"a/b": 1})
	l.put(liveT0.Add(3*time.Second), Sample{"a/b": 2})
	_, _, s := l.snapshot(liveT0)
	if s["a/b"][179] != 2 {
		t.Fatalf("got %v", s["a/b"][179])
	}
}

func TestLiveSeriesCap(t *testing.T) {
	l := newLiveRing()
	smp := Sample{}
	for i := 0; i <= MaxSeries; i++ {
		smp[fmt.Sprintf("c/s%d", i)] = 1
	}
	l.put(liveT0, smp)
	_, _, s := l.snapshot(liveT0)
	if len(s) != MaxSeries {
		t.Fatalf("series=%d", len(s))
	}
}

func TestLiveAbsentKeyOverwritesStale(t *testing.T) {
	l := newLiveRing()
	l.put(liveT0, Sample{"a/b": 7, "c/d": 1})
	wrap := liveT0.Add(LiveSlots * 5 * time.Second) // same ring index, next lap
	l.put(wrap, Sample{"c/d": 2})
	_, _, s := l.snapshot(wrap)
	if !math.IsNaN(s["a/b"][179]) || s["c/d"][179] != 2 {
		t.Fatalf("a/b=%v c/d=%v", s["a/b"][179], s["c/d"][179])
	}
	if !math.IsNaN(s["a/b"][0]) {
		t.Fatalf("stale slot visible: %v", s["a/b"][0])
	}
}

func TestLiveReset(t *testing.T) {
	l := newLiveRing()
	l.put(liveT0, Sample{"a/b": 1})
	l.reset()
	if _, _, s := l.snapshot(liveT0); len(s) != 0 {
		t.Fatalf("series after reset: %d", len(s))
	}
}

func TestLiveWrappedSlotNotReadAsCurrent(t *testing.T) {
	l := newLiveRing()
	l.put(liveT0, Sample{"a/b": 7})
	_, _, s := l.snapshot(liveT0.Add(LiveSlots * 5 * time.Second))
	for i, v := range s["a/b"] {
		if !math.IsNaN(v) {
			t.Fatalf("slot %d reads stale %v", i, v)
		}
	}
}
