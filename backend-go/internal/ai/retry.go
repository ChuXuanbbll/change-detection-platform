package ai

import (
	"context"
	"math/rand"
	"time"
)

const maxRetryAttempts = 3

// RetryNotice describes the next attempt without exposing provider diagnostics.
type RetryNotice struct {
	Stage       string `json:"stage"`
	Attempt     int    `json:"attempt"`
	MaxAttempts int    `json:"max_attempts"`
	DelayMS     int64  `json:"delay_ms"`
}

type retryObserverKey struct{}

func withRetryObserver(ctx context.Context, callback func(RetryNotice)) context.Context {
	return context.WithValue(ctx, retryObserverKey{}, callback)
}

func observeRetry(ctx context.Context, stage string, nextAttempt int, delay time.Duration) {
	if callback, ok := ctx.Value(retryObserverKey{}).(func(RetryNotice)); ok && callback != nil {
		callback(RetryNotice{Stage: stage, Attempt: nextAttempt, MaxAttempts: maxRetryAttempts, DelayMS: delay.Milliseconds()})
	}
}

// Per-instance hooks keep retry tests fast without changing production globals.
type retryTiming struct {
	wait    func(context.Context, time.Duration) error
	backoff func(int) time.Duration
}

func (r retryTiming) delay(failedAttempt int) time.Duration {
	if r.backoff != nil {
		return r.backoff(failedAttempt)
	}
	base := time.Second << (failedAttempt - 1)
	return time.Duration(float64(base) * (0.8 + rand.Float64()*0.4))
}

func (r retryTiming) sleep(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.wait != nil {
		return r.wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
