package stats

import "strings"

// SplitKey splits "chart/name" at the first slash; a key without slash is all chart.
func SplitKey(key string) (chart, name string) {
	chart, name, _ = strings.Cut(key, "/")
	return chart, name
}

var units = map[string]string{
	"cpu/user": "percent", "cpu/system": "percent", "cpu/iowait": "percent", "cpu/throttled": "percent", "cpu/total": "percent",
	"cpu/vcpus":  "count",
	"memory/rss": "bytes", "memory/cache": "bytes", "memory/swap": "bytes", "memory/limit": "bytes",
	"memory/assigned": "bytes", "memory/used": "bytes",
	"network/rx": "bytes_per_second", "network/tx": "bytes_per_second", "network/dropped": "packets_per_second",
	"disk/read": "bytes_per_second", "disk/write": "bytes_per_second",
	"disk/iops_read": "ops_per_second", "disk/iops_write": "ops_per_second",
	"fs/free_percent": "percent",
	"pids/count":      "count", "pids/fds": "count", "restarts/count": "count",
	"health/ok":      "flag",
	"alert/cpu_high": "percent", "alert/mem_high": "percent",
}

// Unit returns the unit of a series; the ":suffix" of name (interface, disk, mount) is ignored.
// Unknown series give "".
func Unit(chart, name string) string {
	name, _, _ = strings.Cut(name, ":")
	return units[chart+"/"+name]
}
