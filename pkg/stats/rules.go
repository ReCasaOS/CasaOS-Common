package stats

import (
	"slices"
	"strconv"
)

// Rule is one alert setting; Key is "cpu_high" or "mem_high".
type Rule struct {
	Key       string  `json:"key"`
	Enabled   bool    `json:"enabled"`
	Threshold float64 `json:"threshold"`
	Minutes   int     `json:"minutes"`
}

// ruleKeys are the alert keys, in the order Rules returns them.
var ruleKeys = []string{"cpu_high", "mem_high"}

// DefaultRules returns cpu_high 90 % for 10 min and mem_high 90 % for 5 min, both enabled.
func DefaultRules() []Rule {
	return []Rule{
		{Key: "cpu_high", Enabled: true, Threshold: 90, Minutes: 10},
		{Key: "mem_high", Enabled: true, Threshold: 90, Minutes: 5},
	}
}

// validateRules checks a rule list: known keys, no duplicate, threshold 1..100, minutes 1..120.
func validateRules(rules []Rule) error {
	for i, r := range rules {
		switch {
		case !slices.Contains(ruleKeys, r.Key):
			return &RuleError{i, "key", "must be cpu_high or mem_high"}
		case slices.ContainsFunc(rules[:i], func(p Rule) bool { return p.Key == r.Key }):
			return &RuleError{i, "key", "duplicate"}
		case !(r.Threshold >= 1 && r.Threshold <= 100): // also rejects NaN
			return &RuleError{i, "threshold", "must be between 1 and 100"}
		case r.Minutes < 1 || r.Minutes > 120:
			return &RuleError{i, "minutes", "must be between 1 and 120"}
		}
	}
	return nil
}

// RuleError names the faulty field of a rule list (route error bad_rules).
type RuleError struct {
	Index          int
	Field, Message string
}

func (e *RuleError) Error() string {
	return "rules[" + strconv.Itoa(e.Index) + "]." + e.Field + ": " + e.Message
}
