// Package disk holds the functions that look at disks and filesystems.
// Each file registers its function from init().
package disk

import (
	"context"
	"errors"

	"verifien/spec"
)

func init() {
	spec.Register("disk", spec.FuncSpec{
		Run:     run,
		Async:   true, // assumed slow (remote disk)
		Options: spec.StdOptions(),
		Tests: map[string]spec.Field{
			"free": {Kind: spec.KSize},
			"used": {Kind: spec.KSize},
		},
	})
}

func run(ctx context.Context, req spec.Request) (spec.Result, error) {
	// A remote disk can hang, so the blocking call runs through Attempt, which
	// puts it in a goroutine under req.Timeout and retries errors.
	return spec.Attempt(ctx, req, func() (spec.Result, error) {
		// PLACEHOLDER: replace with the real call against req.Target.
		// It must report "free" and "used" in bytes as int64.
		return nil, errors.New("disk: not implemented yet")
	})
}
