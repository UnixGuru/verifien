// Package shell holds the functions that run commands. Each file registers its
// function from init(). Unix only: the command is run by sh -c, in its own
// process group (see command).
package shell

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"verifien/spec"
)

// maxOutput is how much of stdout, and of stderr, is kept. A command that writes
// more is not stopped (it would block on a full pipe); the rest is dropped.
const maxOutput = 1 << 20

func init() {
	spec.Register("cmd", spec.FuncSpec{
		Run: cmd,
		// Not Async: a command can do things, so it runs when its own statement is
		// reached, never ahead of an earlier `if` that decides whether it should.
		Options: spec.StdOptions(),
		// A command can have effects, so it is run again only when the call asks
		// for it: cmd("script", {retry 2}). [DEFAULTS] retry does not apply.
		NoDefaultRetry: true,
		Tests: map[string]spec.Field{
			"exitstatus": {Kind: spec.KInt}, // 0-255
			"stdout":     {Kind: spec.KString},
			"stderr":     {Kind: spec.KString},
		},
	})
}

// cmd runs the one-line command in the target string with `sh -c` and reports
// its exit status and what it wrote.
//
// An attempt succeeds when the command exits with status 0. Anything else (a
// non-zero status, a command that cannot be started, one killed by a signal, one
// that runs past the timeout) is a failed attempt, and is run again if the call
// has a retry, after `sleep`. The default is no retry (see NoDefaultRetry).
// When the attempts run out, a command that last exited non-zero is still a
// result, so exitstatus == 3 can be tested; the other failures are errors.
//
// stdin is /dev/null. stdout and stderr lose their trailing newlines, as in
// $(command), so `stdout == "active"` matches "active\n".
func cmd(ctx context.Context, req spec.Request) (spec.Result, error) {
	if strings.TrimSpace(req.Target) == "" {
		return nil, errors.New("cmd: the command is empty")
	}
	// spec.Attempt runs each attempt in its own goroutine and gives up on it at the
	// timeout, so a hung command never holds up the section; once() also kills the
	// command (and anything it started) when the timeout passes.
	res, err := spec.Attempt(ctx, req, func() (spec.Result, error) {
		res, err := once(ctx, req)
		if err == nil && res["exitstatus"] != int64(0) {
			return nil, &nonZero{res} // fails the attempt, but keeps what it said
		}
		return res, err
	})
	var nz *nonZero
	if errors.As(err, &nz) {
		return nz.res, nil
	}
	return res, err
}

// nonZero is the error of an attempt whose command exited with a non-zero status.
type nonZero struct{ res spec.Result }

func (e *nonZero) Error() string { return fmt.Sprintf("exit status %d", e.res["exitstatus"]) }

func once(ctx context.Context, req spec.Request) (spec.Result, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	var stdout, stderr capped
	c := command(ctx, req.Target)
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()

	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, fmt.Errorf("cmd: %v", err) // could not be started
		}
		if exit.ExitCode() < 0 { // killed by a signal
			if ctx.Err() != nil {
				return nil, fmt.Errorf("cmd: timed out after %s and was stopped", req.Timeout)
			}
			return nil, fmt.Errorf("cmd: %v", exit)
		}
	}
	return spec.Result{
		"exitstatus": int64(c.ProcessState.ExitCode()),
		"stdout":     strings.TrimRight(stdout.String(), "\r\n"),
		"stderr":     strings.TrimRight(stderr.String(), "\r\n"),
	}, nil
}

// command builds sh -c line. It puts the command in its own process group and
// makes a cancel (the timeout) kill the whole group, so a child the command
// started (sleep, curl, ...) does not outlive it. WaitDelay stops us waiting for
// output pipes that a surviving background child still holds.
func command(ctx context.Context, line string) *exec.Cmd {
	c := exec.CommandContext(ctx, "sh", "-c", line)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 2 * time.Second
	return c
}

// capped is a writer that keeps the first maxOutput bytes and accepts the rest.
type capped struct{ b []byte }

func (w *capped) Write(p []byte) (int, error) {
	if room := maxOutput - len(w.b); room > 0 {
		w.b = append(w.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (w *capped) String() string { return string(w.b) }
