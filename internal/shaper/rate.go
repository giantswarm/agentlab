package shaper

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseRate reads a rate in bits per second: a number with an optional unit
// bit, kbit, mbit or gbit (k, m and g for short), case-insensitive. Byte units
// are refused on purpose: tc's "mbps" means megabytes.
func ParseRate(s string) (float64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	units := []struct {
		suffix string
		mult   float64
	}{
		{"gbit", 1e9}, {"mbit", 1e6}, {"kbit", 1e3}, {"bit", 1},
		{"g", 1e9}, {"m", 1e6}, {"k", 1e3},
	}
	mult := 1.0
	for _, u := range units {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v <= 0 || math.IsInf(v*mult, 0) || math.IsNaN(v) {
		return 0, fmt.Errorf("invalid rate %q (want e.g. 500kbit, 20mbit, 1gbit)", s)
	}
	return v * mult, nil
}

// FormatRate prints a rate for people; 0 means unlimited.
func FormatRate(bps float64) string {
	switch {
	case bps <= 0:
		return "unlimited"
	case bps >= 1e9:
		return fmt.Sprintf("%.2f Gbit/s", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.1f Mbit/s", bps/1e6)
	default:
		return fmt.Sprintf("%.0f kbit/s", bps/1e3)
	}
}

// formatThroughput is FormatRate for measured throughput, where 0 is idle.
func formatThroughput(bps float64) string {
	if bps < 1e3 {
		return "nothing"
	}
	return FormatRate(bps)
}

// RateArg prints a rate the way ParseRate reads it, in the largest unit that
// keeps it whole: 5mbit, 1500kbit.
func RateArg(bps float64) string {
	for _, u := range []struct {
		suffix string
		mult   float64
	}{{"gbit", 1e9}, {"mbit", 1e6}, {"kbit", 1e3}} {
		if v := bps / u.mult; v >= 1 && v == math.Trunc(v) {
			return strconv.FormatFloat(v, 'f', -1, 64) + u.suffix
		}
	}
	return strconv.FormatFloat(bps, 'f', -1, 64) + "bit"
}
