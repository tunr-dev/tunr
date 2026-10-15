package bridge

import (
	"testing"
	"time"
)

func TestCron(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		spec, when string
		want       bool
	}{
		{"0 7 * * *", "2026-10-15 07:00", true},
		{"0 7 * * *", "2026-10-15 07:01", false},
		{"*/15 * * * *", "2026-10-15 13:45", true},
		{"*/15 * * * *", "2026-10-15 13:44", false},
		{"0 9 * * 1-5", "2026-10-15 09:00", true},  // Thursday
		{"0 9 * * 1-5", "2026-10-18 09:00", false}, // Sunday
		{"0 0 * * 0", "2026-10-18 00:00", true},
		{"0 0 * * 7", "2026-10-18 00:00", true}, // 7 = Sunday
		{"0 0 1 * *", "2026-11-01 00:00", true},
		{"0 0 1 * 1", "2026-10-19 00:00", true}, // Vixie: dom OR dow
		{"30 2 1,15 * *", "2026-10-15 02:30", true},
		{"5-10/5 * * * *", "2026-10-15 02:10", true},
		{"5-10/5 * * * *", "2026-10-15 02:07", false},
	}
	for _, c := range cases {
		cr, err := ParseCron(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if got := cr.Matches(at(c.when)); got != c.want {
			t.Errorf("%q at %s = %v, want %v", c.spec, c.when, got, c.want)
		}
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8",
		"*/0 * * * *", "a * * * *", "5-1 * * * *", "@hourly", "* * * * * *"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("ParseCron(%q) accepted", bad)
		}
	}
}
