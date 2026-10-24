// Package env holds the functions that look at the process environment.
// Each file registers its function from init().
package env

import (
	"context"
	"errors"
	"os"

	"verifien/spec"
)

func init() {
	spec.Register("environ", spec.FuncSpec{
		Run: environ,
		// The standard options are accepted so a config can carry the same
		// options on every function, but environ ignores timeout, retry and
		// sleep: it never blocks and always makes exactly one attempt. Only
		// msg has any effect.
		Options: spec.StdOptions(),
		Tests: map[string]spec.Field{
			"set":   {Kind: spec.KEnum, Enum: []string{"yes", "no"}, Ops: []string{"==", "!="}},
			"value": {Kind: spec.KString}, // "" when the variable is not set
		},
	})
}

// environ looks up the variable named by the target string. It does not use
// spec.Attempt: there is nothing to wait for, so no goroutine, no timeout, no
// retries and no sleep.
func environ(ctx context.Context, req spec.Request) (spec.Result, error) {
	if req.Target == "" {
		return nil, errors.New("environ: variable name is empty")
	}
	v, ok := os.LookupEnv(req.Target)
	set := "no"
	if ok {
		set = "yes"
	}
	return spec.Result{"set": set, "value": v}, nil
}
