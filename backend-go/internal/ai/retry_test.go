package ai

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryTimingJitterBoundsAndCancellation(t *testing.T) {
	timing := retryTiming{}
	for attempt := 1; attempt <= 2; attempt++ {
		base := time.Second << (attempt - 1)
		for sample := 0; sample < 100; sample++ {
			got := timing.delay(attempt)
			if got < base*8/10 || got > base*12/10 {
				t.Fatalf("attempt=%d delay=%s", attempt, got)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := timing.sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancellation did not stop wait")
	}
}
