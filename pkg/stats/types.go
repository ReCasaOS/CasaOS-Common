// Package stats stores per-item resource statistics: 15 minutes at 5 s in
// memory and 7 days of minutes (avg, min, max per series) in one fixed-size
// ring file per item. A query never waits for an item's writer (it reads the
// ring file without the writer's lock); it takes the store read lock for the
// lookup, so it can wait behind the creation of an item and behind Forget and
// Sweep, which hold the store lock. Series keys must be stable: a ring never
// frees a series slot, so after MaxSeries distinct keys every new key is
// rejected for the life of the item. An error from Record for which IsPartial
// is true means the other keys of the sample were stored. One known limit: after a clock step
// back of more than a minute, a query overlapping the rewrite of the same
// minute may mix that minute's columns in one point of one response.
//
// The directory is 0750 and the files 0640: they name containers, VMs and
// interfaces.
package stats

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	Minutes       = 10080 // slots in a ring file (7 days of minutes)
	LiveSlots     = 180   // 15 minutes at 5 s
	MaxSeries     = 48    // series per item
	MaxItems      = 256   // live items
	MaxForgotten  = 256   // forgotten items kept
	MaxKeyLen     = 63    // bytes of a series key
	MaxItemLen    = 256   // bytes of an item name
	FormatVersion = 1
	// MaxFileSize is the size of a ring file holding MaxSeries series:
	// 8192 header bytes + 40320 bytes per column x (1 stamp + 3 x MaxSeries).
	MaxFileSize = 8192 + 40320*(1+3*MaxSeries)
)

var (
	ErrUnknownItem  = errors.New("stats: unknown item")
	ErrBadRange     = errors.New("stats: bad range")
	ErrBadItem      = errors.New("stats: bad item name")
	ErrBadKey       = errors.New("stats: bad series key")
	ErrSeriesCap    = errors.New("stats: too many series")
	ErrTooManyItems = errors.New("stats: too many items")
	ErrOutOfOrder   = errors.New("stats: sample out of order")
	ErrClosed       = errors.New("stats: store closed")
	ErrRingSkipped  = errors.New("stats: ring file unreadable at Open, left untouched") // the error text names the file
)

// Range is a period asked by the page.
type Range string

const (
	RangeLive Range = "live"
	Range15m  Range = "15m"
	Range1h   Range = "1h"
	Range24h  Range = "24h"
	Range7d   Range = "7d"
)

// ParseRange accepts exactly the five lower-case period names.
func ParseRange(s string) (Range, bool) {
	switch r := Range(s); r {
	case RangeLive, Range15m, Range1h, Range24h, Range7d:
		return r, true
	}
	return "", false
}

// Sample is a measure at one instant: "chart/name" -> value. An absent key means no data, never 0.
type Sample map[string]float64

// Point: T in Unix seconds; nil Avg/Min/Max is a gap.
type Point struct {
	T             int64
	Avg, Min, Max *float64
}

// MarshalJSON writes [t, avg, min, max] with null for gaps.
func (p Point) MarshalJSON() ([]byte, error) {
	return json.Marshal([4]any{p.T, p.Avg, p.Min, p.Max})
}

func (p *Point) UnmarshalJSON(b []byte) error {
	var a []json.RawMessage
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	if len(a) != 4 {
		return fmt.Errorf("stats: point has %d elements, want 4", len(a))
	}
	if err := json.Unmarshal(a[0], &p.T); err != nil {
		return err
	}
	for i, dst := range []**float64{&p.Avg, &p.Min, &p.Max} {
		*dst = nil
		if err := json.Unmarshal(a[i+1], dst); err != nil {
			return err
		}
	}
	return nil
}

type SeriesData struct {
	Chart  string  `json:"chart"`
	Name   string  `json:"name"`
	Unit   string  `json:"unit"`
	Points []Point `json:"points"`
}

type Result struct {
	Range       Range        `json:"range"`
	StepSeconds int          `json:"step_seconds"`
	From        int64        `json:"from"`
	To          int64        `json:"to"`
	Series      []SeriesData `json:"series"` // never nil: empty gives "series":[]
}

// MarshalJSON writes a nil Series as [].
func (r Result) MarshalJSON() ([]byte, error) {
	type plain Result
	if r.Series == nil {
		r.Series = []SeriesData{}
	}
	return json.Marshal(plain(r))
}

// IsPartial reports whether err from Record only says that some keys of the
// sample were rejected (ErrSeriesCap, ErrBadKey): the other keys were stored,
// so the caller must neither retry nor drop the sample. It is false for nil and
// for any error that has another cause, alone or joined.
func IsPartial(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrSeriesCap) || errors.Is(err, ErrBadKey) {
		if j, ok := err.(interface{ Unwrap() []error }); ok {
			for _, e := range j.Unwrap() {
				if !IsPartial(e) {
					return false
				}
			}
		}
		return true
	}
	return false
}
