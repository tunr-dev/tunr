package bridge

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	l := newLimiter(2, 3)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("a", now); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	ok, wait := l.allow("a", now)
	if ok || wait <= 0 {
		t.Fatal("4th request in a burst of 3 allowed")
	}
	if ok, _ := l.allow("b", now); !ok {
		t.Error("another principal was throttled")
	}
	if ok, _ := l.allow("a", now.Add(600*time.Millisecond)); !ok {
		t.Error("bucket did not refill at 2/s")
	}
}
