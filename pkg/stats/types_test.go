package stats

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRange(t *testing.T) {
	for _, s := range []string{"live", "15m", "1h", "24h", "7d"} {
		r, ok := ParseRange(s)
		assert.True(t, ok, s)
		assert.Equal(t, Range(s), r)
	}
	for _, s := range []string{"", "1d", "LIVE", "7D"} {
		r, ok := ParseRange(s)
		assert.False(t, ok, s)
		assert.Equal(t, Range(""), r)
	}
}

func TestPointJSON(t *testing.T) {
	for _, c := range []struct {
		p    Point
		want string
	}{
		{Point{T: 60, Avg: f(1.5), Min: f(1), Max: f(2)}, `[60,1.5,1,2]`},
		{Point{T: 60}, `[60,null,null,null]`},
	} {
		b, err := json.Marshal(c.p)
		require.NoError(t, err)
		assert.Equal(t, c.want, string(b))
		var back Point
		require.NoError(t, json.Unmarshal(b, &back))
		assert.Equal(t, c.p, back)
	}
}

func TestPointJSONRejectsWrongLength(t *testing.T) {
	for _, in := range []string{`[]`, `[60,1,2]`, `[60,1,2,3,4]`} {
		var p Point
		assert.Error(t, json.Unmarshal([]byte(in), &p), in)
	}
}

func TestResultNilSeriesJSON(t *testing.T) {
	b, err := json.Marshal(Result{Range: Range7d})
	require.NoError(t, err)
	assert.Equal(t, `{"range":"7d","step_seconds":0,"from":0,"to":0,"series":[]}`, string(b))
}

func TestResultJSON(t *testing.T) {
	r := Result{Range: Range1h, StepSeconds: 60, From: 0, To: 60, Series: []SeriesData{
		{Chart: "cpu", Name: "user", Unit: "percent", Points: []Point{{T: 60, Avg: f(1)}}},
	}}
	b, err := json.Marshal(r)
	require.NoError(t, err)
	assert.Equal(t, `{"range":"1h","step_seconds":60,"from":0,"to":60,"series":[{"chart":"cpu","name":"user","unit":"percent","points":[[60,1,null,null]]}]}`, string(b))

	b, err = json.Marshal(Rule{Key: "cpu_high", Enabled: true, Threshold: 90, Minutes: 10})
	require.NoError(t, err)
	assert.Equal(t, `{"key":"cpu_high","enabled":true,"threshold":90,"minutes":10}`, string(b))
}

func TestFakeClock(t *testing.T) {
	c := NewFakeClock(t0)
	assert.True(t, c.Now().Equal(t0))
	c.Advance(90 * time.Second)
	assert.True(t, c.Now().Equal(t0.Add(90*time.Second)))
	c.Set(t0.Add(time.Hour))
	assert.True(t, c.Now().Equal(t0.Add(time.Hour)))
	var _ Clock = SystemClock{}
}
