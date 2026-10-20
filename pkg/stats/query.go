package stats

import (
	"errors"
	"math"
	"slices"
)

// floorTo rounds x down to a multiple of m, negatives included.
func floorTo(x, m int64) int64 { return x - ((x%m)+m)%m }

// Query returns item over r. It takes no lock a writer holds while writing:
// the store read lock for the lookup, then plain reads of the ring file
// (checked by its stamps) and the live ring under its own read lock.
func (s *Store) Query(item string, r Range) (Result, error) {
	var step, n int64
	switch r {
	case RangeLive:
		step, n = liveStep, LiveSlots
	case Range15m:
		step, n = 60, 15
	case Range1h:
		step, n = 60, 60
	case Range24h:
		step, n = 300, 288
	case Range7d:
		step, n = 1800, 336
	default:
		return Result{}, ErrBadRange
	}
	s.mu.RLock()
	e, closed := s.items[item], s.closed
	s.mu.RUnlock()
	if closed {
		return Result{}, ErrClosed
	}
	if e == nil {
		return Result{}, ErrUnknownItem
	}
	now := s.clock.Now()
	res := Result{Range: r, StepSeconds: int(step), Series: []SeriesData{}}
	points := map[string][]Point{}
	if r == RangeLive {
		from, to, live := e.live.snapshot(now)
		res.From, res.To = from, to
		for k, vals := range live {
			pts := make([]Point, n)
			for i, v := range vals {
				pts[i].T = from + int64(i)*step
				if !math.IsNaN(v) {
					a, lo, hi := v, v, v
					pts[i].Avg, pts[i].Min, pts[i].Max = &a, &lo, &hi
				}
			}
			points[k] = pts
		}
	} else {
		lastMin := floorTo(now.Unix(), 60) - 60
		res.To = floorTo(lastMin, step)
		res.From = res.To - (n-1)*step
		per := step / 60
		cells, err := e.ring.readWindow(res.From/60, res.To/60+per-1)
		if errors.Is(err, errRingClosed) { // closed since the lookup
			s.mu.RLock()
			closed = s.closed
			s.mu.RUnlock()
			if closed {
				return Result{}, ErrClosed
			}
			return Result{}, ErrUnknownItem
		}
		if err != nil {
			return Result{}, err
		}
		for k, c := range cells {
			points[k] = buckets(c, res.From, step, n)
		}
	}
	keys := make([]string, 0, len(points))
	for k := range points {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		chart, name := SplitKey(k)
		pts := points[k]
		if chart == "alert" || !slices.ContainsFunc(pts, func(p Point) bool { return p.Avg != nil }) {
			continue
		}
		res.Series = append(res.Series, SeriesData{Chart: chart, Name: name, Unit: Unit(chart, name), Points: pts})
	}
	return res, nil
}

// buckets folds minute cells into n points of step seconds starting at from:
// mean of the minute averages, min of minimums, max of maximums; a bucket
// without any minute is a gap.
func buckets(cells []minuteCell, from, step, n int64) []Point {
	per := step / 60
	pts := make([]Point, n)
	for i := range pts {
		pts[i].T = from + int64(i)*step
		var sum, lo, hi float64
		cnt := 0
		for _, c := range cells[int64(i)*per : int64(i+1)*per] {
			if !c.OK {
				continue
			}
			if cnt == 0 {
				lo, hi = c.Min, c.Max
			}
			sum, lo, hi = sum+c.Avg, min(lo, c.Min), max(hi, c.Max)
			cnt++
		}
		if cnt > 0 {
			avg := sum / float64(cnt)
			pts[i].Avg, pts[i].Min, pts[i].Max = &avg, &lo, &hi
		}
	}
	return pts
}
