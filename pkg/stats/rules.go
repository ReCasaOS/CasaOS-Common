package stats

import "strconv"

// Rule is one alert setting; Key is "cpu_high" or "mem_high".
type Rule struct {
	Key       string  `json:"key"`
	Enabled   bool    `json:"enabled"`
	Threshold float64 `json:"threshold"`
	Minutes   int     `json:"minutes"`
}

// RuleError names the faulty field of a rule list (route error bad_rules).
type RuleError struct {
	Index          int
	Field, Message string
}

func (e *RuleError) Error() string {
	return "rules[" + strconv.Itoa(e.Index) + "]." + e.Field + ": " + e.Message
}
