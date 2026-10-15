package bridge

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cron.go — a small 5-field cron (minute hour day-of-month month day-of-week),
// evaluated in UTC. Supports *, lists (1,5), ranges (1-5), steps (*/15, 1-30/5)
// and day-of-week 0-7 (0 and 7 are Sunday). No names, no seconds, no "@hourly":
// what we don't parse, we refuse.

// Cron is a parsed schedule.
type Cron struct {
	min, hour, dom, mon, dow uint64 // bitsets
	domStar, dowStar         bool
}

// ParseCron parses a 5-field expression.
func ParseCron(spec string) (*Cron, error) {
	f := strings.Fields(spec)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron needs 5 fields (minute hour day month weekday), got %d", len(f))
	}
	var c Cron
	var err error
	if c.min, err = cronField(f[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if c.hour, err = cronField(f[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if c.dom, err = cronField(f[2], 1, 31); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if c.mon, err = cronField(f[3], 1, 12); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if c.dow, err = cronField(f[4], 0, 7); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	if c.dow&(1<<7) != 0 { // 7 = Sunday
		c.dow |= 1
	}
	c.domStar, c.dowStar = f[2] == "*", f[4] == "*"
	return &c, nil
}

func cronField(s string, lo, hi int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("bad step %q", stepStr)
			}
			step = n
		}
		a, b := lo, hi
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			x, y, _ := strings.Cut(rng, "-")
			var err1, err2 error
			a, err1 = strconv.Atoi(x)
			b, err2 = strconv.Atoi(y)
			if err1 != nil || err2 != nil {
				return 0, fmt.Errorf("bad range %q", rng)
			}
		default:
			n, err := strconv.Atoi(rng)
			if err != nil {
				return 0, fmt.Errorf("bad value %q", rng)
			}
			a, b = n, n
			if hasStep {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return 0, fmt.Errorf("%q out of range %d-%d", rng, lo, hi)
		}
		for v := a; v <= b; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

// Matches reports whether t (in UTC, to the minute) is a scheduled time.
// Day matching follows Vixie cron: if both day fields are restricted, either
// may match.
func (c *Cron) Matches(t time.Time) bool {
	t = t.UTC()
	if c.min&(1<<uint(t.Minute())) == 0 || c.hour&(1<<uint(t.Hour())) == 0 || c.mon&(1<<uint(t.Month())) == 0 {
		return false
	}
	dom := c.dom&(1<<uint(t.Day())) != 0
	dow := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	default:
		return dom || dow
	}
}
