package stats

import (
	"errors"
	"time"
)

// Store is the per-item history store. Only the shape needed by the shared test
// helpers exists so far; the implementation follows in the storage tasks.
type Store struct{}

// Record adds one sample of item taken at the given time.
func (s *Store) Record(item string, at time.Time, smp Sample) error {
	return errors.New("stats: not implemented")
}
