package stats

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitKey(t *testing.T) {
	for in, want := range map[string][2]string{
		"cpu/user":                  {"cpu", "user"},
		"network/rx:eth0":           {"network", "rx:eth0"},
		"fs/free_percent:/mnt/data": {"fs", "free_percent:/mnt/data"},
		"nochart":                   {"nochart", ""},
	} {
		c, n := SplitKey(in)
		assert.Equal(t, want, [2]string{c, n}, in)
	}
}

func TestUnit(t *testing.T) {
	for unit, keys := range map[string][][2]string{
		"percent":            {{"cpu", "user"}, {"cpu", "system"}, {"cpu", "iowait"}, {"cpu", "throttled"}, {"cpu", "total"}, {"fs", "free_percent:/"}, {"alert", "cpu_high"}, {"alert", "mem_high"}},
		"count":              {{"cpu", "vcpus"}, {"pids", "count"}, {"pids", "fds"}, {"restarts", "count"}},
		"bytes":              {{"memory", "rss"}, {"memory", "cache"}, {"memory", "swap"}, {"memory", "limit"}, {"memory", "assigned"}, {"memory", "used"}},
		"bytes_per_second":   {{"network", "rx:eth0"}, {"network", "tx:eth0"}, {"disk", "read"}, {"disk", "write"}, {"disk", "read:vda"}},
		"packets_per_second": {{"network", "dropped:eth0"}},
		"ops_per_second":     {{"disk", "iops_read"}, {"disk", "iops_write"}},
		"flag":               {{"health", "ok"}},
		"":                   {{"nope", "x"}, {"cpu", "nope"}, {"", ""}},
	} {
		for _, k := range keys {
			assert.Equal(t, unit, Unit(k[0], k[1]), k[0]+"/"+k[1])
		}
	}
}
