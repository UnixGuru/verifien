// Package actionscript is the as:IDENT action: it runs a one-line shell script
// from [ACTION_SCRIPTS] and writes the failure report to its stdin as JSON. The
// shell is sh -c (Unix only; see proc.go).
package actionscript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"verifien/spec"
)

func init() {
	// Registered as "as"; the runner splits "as:cleanup" into this name and the
	// IDENT, and fills in req.Script from [ACTION_SCRIPTS].
	spec.RegisterAction("as", spec.ActionSpec{Run: run})
}

func run(ctx context.Context, req spec.ActionRequest) error {
	if strings.TrimSpace(req.Script) == "" {
		return fmt.Errorf("as:%s has no script", req.Arg)
	}
	payload, err := json.Marshal(req.Report)
	if err != nil {
		return err
	}
	// Each attempt starts a fresh process. The attempt's context kills it when
	// the timeout passes, so a hung script does not linger.
	return req.Attempt(ctx, func(actx context.Context) error {
		cmd := command(actx, req.Script) // sh -c; see proc.go
		cmd.Stdin = bytes.NewReader(payload)
		cmd.WaitDelay = 2 * time.Second // do not wait forever on pipes a child still holds
		out, err := cmd.CombinedOutput()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return fmt.Errorf("as:%s: %v%s", req.Arg, err, tail(out))
			}
			return fmt.Errorf("as:%s: %w", req.Arg, err)
		}
		return nil
	})
}

// tail returns ": <last of the script's output>" for an error message.
func tail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	if len(s) > 200 {
		s = "..." + s[len(s)-200:]
	}
	return ": " + s
}
