package stats_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
)

func TestDefaultRules(t *testing.T) {
	want := []stats.Rule{
		{Key: "cpu_high", Enabled: true, Threshold: 90, Minutes: 10},
		{Key: "mem_high", Enabled: true, Threshold: 90, Minutes: 5},
	}
	if got := stats.DefaultRules(); !slices.Equal(got, want) {
		t.Fatalf("DefaultRules() = %+v, want %+v", got, want)
	}
	b, err := json.Marshal(stats.DefaultRules()[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"key":"cpu_high","enabled":true,"threshold":90,"minutes":10}` {
		t.Fatalf("json = %s", got)
	}
}
