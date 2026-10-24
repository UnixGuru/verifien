package spec

import (
	"context"
	"fmt"
	"time"
)

// Attempt runs op, a blocking call that cannot be cancelled (os.Lstat, a
// syscall, a network request), under the settings in req:
//
//   - every attempt runs in its own goroutine and is abandoned if it takes
//     longer than req.Timeout (0 or less means no timeout);
//   - the call runs once, plus every retry: an attempt that fails or times out
//     is retried, up to req.Retry more times, so there are at most req.Retry+1
//     attempts in all;
//   - req.Sleep is the pause between attempts (none before the first, none after
//     the last);
//   - it returns at once if ctx is cancelled.
//
// It returns the first successful result, or, only once every attempt is used
// up, the error of the last attempt.
// An abandoned attempt keeps running in the background until op returns. If op
// never returns, that goroutine is leaked, but the caller is not held up.
func Attempt[T any](ctx context.Context, req Request, op func() (T, error)) (T, error) {
	attempts := req.Retry + 1
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		var v T
		v, err = attemptOnce(ctx, req.Timeout, op)
		if err == nil {
			return v, nil
		}
		if ctx.Err() != nil {
			break
		}
		if i < attempts-1 && req.Sleep > 0 {
			select {
			case <-time.After(req.Sleep):
			case <-ctx.Done():
				var zero T
				return zero, ctx.Err()
			}
		}
	}
	var zero T
	return zero, err
}

type outcome[T any] struct {
	v   T
	err error
}

func attemptOnce[T any](ctx context.Context, timeout time.Duration, op func() (T, error)) (T, error) {
	done := make(chan outcome[T], 1) // buffered, so an abandoned attempt can still finish
	go func() {
		v, err := op()
		done <- outcome[T]{v, err}
	}()

	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}

	var zero T
	select {
	case o := <-done:
		return o.v, o.err
	case <-expired:
		return zero, fmt.Errorf("timed out after %s", timeout)
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}
