package spec

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Attempt runs the call once plus every retry, and only then returns the error.

func TestAttemptRetriesErrors(t *testing.T) {
	var n int32
	_, err := Attempt(context.Background(), Request{Timeout: time.Second, Retry: 2}, func() (int, error) {
		atomic.AddInt32(&n, 1)
		return 0, errors.New("boom")
	})
	assert.Error(t, err)
	assert.EqualValues(t, 3, atomic.LoadInt32(&n), "1 attempt + 2 retries")
}

func TestAttemptStopsAtFirstSuccess(t *testing.T) {
	var n int32
	v, err := Attempt(context.Background(), Request{Timeout: time.Second, Retry: 3}, func() (int, error) {
		if atomic.AddInt32(&n, 1) < 3 {
			return 0, errors.New("not yet")
		}
		return 7, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 7, v)
	assert.EqualValues(t, 3, atomic.LoadInt32(&n))
}

func TestAttemptNoRetryMeansOneAttempt(t *testing.T) {
	var n int32
	Attempt(context.Background(), Request{Timeout: time.Second}, func() (int, error) {
		atomic.AddInt32(&n, 1)
		return 0, errors.New("x")
	})
	assert.EqualValues(t, 1, atomic.LoadInt32(&n))
}

func TestAttemptTimeoutIsPerAttempt(t *testing.T) {
	var n int32
	start := time.Now()
	_, err := Attempt(context.Background(), Request{Timeout: 100 * time.Millisecond, Retry: 1}, func() (int, error) {
		atomic.AddInt32(&n, 1)
		time.Sleep(2 * time.Second) // abandoned after 100ms, twice
		return 1, nil
	})
	d := time.Since(start)
	assert.Error(t, err, "want a timeout error")
	assert.EqualValues(t, 2, atomic.LoadInt32(&n))
	assert.Greater(t, d, 190*time.Millisecond)
	assert.Less(t, d, 700*time.Millisecond)
}

func TestAttemptStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	var n int32
	_, err := Attempt(ctx, Request{Timeout: 5 * time.Second, Retry: 5}, func() (int, error) {
		atomic.AddInt32(&n, 1)
		time.Sleep(time.Second)
		return 0, nil
	})
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 400*time.Millisecond)
	assert.EqualValues(t, 1, atomic.LoadInt32(&n))
}

// Sleep is the pause between attempts: not before the first, not after the last.

func TestAttemptSleepsBetweenAttemptsOnly(t *testing.T) {
	var n int32
	start := time.Now()
	_, err := Attempt(context.Background(), Request{Timeout: time.Second, Retry: 2, Sleep: 150 * time.Millisecond}, func() (int, error) {
		atomic.AddInt32(&n, 1)
		return 0, errors.New("x")
	})
	d := time.Since(start)
	// 3 attempts means 2 sleeps.
	assert.Error(t, err)
	assert.EqualValues(t, 3, atomic.LoadInt32(&n))
	assert.Greater(t, d, 290*time.Millisecond)
	assert.Less(t, d, 600*time.Millisecond)
}

func TestAttemptDoesNotSleepAfterSuccess(t *testing.T) {
	start := time.Now()
	Attempt(context.Background(), Request{Retry: 3, Sleep: time.Second}, func() (int, error) { return 1, nil })
	assert.Less(t, time.Since(start), 200*time.Millisecond, "slept after a success")
}

func TestAttemptSleepIsInterruptedByCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := Attempt(ctx, Request{Retry: 3, Sleep: 5 * time.Second}, func() (int, error) { return 0, errors.New("x") })
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}

// ActionRequest.Attempt gives the operation a context that ends at the timeout,
// so it can stop what it started.

func TestActionAttemptCancelsOperationContext(t *testing.T) {
	var sawCancel atomic.Bool
	req := ActionRequest{Timeout: 100 * time.Millisecond}
	err := req.Attempt(context.Background(), func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			sawCancel.Store(true)
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil
		}
	})
	time.Sleep(50 * time.Millisecond)
	assert.Error(t, err, "want a timeout")
	assert.True(t, sawCancel.Load(), "want a cancelled context")
}
