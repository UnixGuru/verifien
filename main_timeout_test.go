package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// timeout is seconds with decimals, the same type as sleep.
func TestTimeoutAcceptsDecimals(t *testing.T) {
	_, set := mustLoad(t, "[DEFAULTS] {timeout 1.5, sleep 0.25, rerun_timeout 2.25}\n[a]\nck cmd(\"true\")")
	assert.Equal(t, 1500*time.Millisecond, set.Timeout)
	assert.Equal(t, 250*time.Millisecond, set.Sleep)
	assert.Equal(t, 2250*time.Millisecond, set.RerunTimeout)
	// whole seconds still work
	_, set = mustLoad(t, "[DEFAULTS] {timeout 3}\n[a]\nck cmd(\"true\")")
	assert.Equal(t, 3*time.Second, set.Timeout)
	// everywhere it can be written
	mustLoad(t, "[a]\nck cmd(\"true\", {timeout 0.5})\nck lstat(\"/\", {timeout 0.001})\naction webhook {url \"http://h/x\", timeout 0.5}\naction rerun {timeout 0.5}\naction as:x {timeout 2.5}\n[ACTION_SCRIPTS]\nas:x = \"true\"")
}

func TestTimeoutMustBeAboveZero(t *testing.T) {
	for name, cfg := range map[string]string{
		"option 0":          "[a]\nck cmd(\"true\", {timeout 0})",
		"option 0.0":        "[a]\nck cmd(\"true\", {timeout 0.0})",
		"option too small":  "[a]\nck cmd(\"true\", {timeout 0.0000000001})",
		"defaults 0.0":      "[DEFAULTS] {timeout 0.0}\n[a]\nck cmd(\"true\")",
		"rerun_timeout 0.0": "[DEFAULTS] {rerun_timeout 0.0}\n[a]\nck cmd(\"true\")",
		"action option 0.0": "[a]\nck cmd(\"true\")\naction webhook {url \"http://h/x\", timeout 0.0}",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := load(cfg)
			require.Error(t, err)
			assert.ErrorContains(t, err, "greater than 0")
		})
	}
	for name, cfg := range map[string]string{
		"text":   "[a]\nck cmd(\"true\", {timeout \"1\"})",
		"suffix": "[a]\nck cmd(\"true\", {timeout 1s})",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := load(cfg)
			assert.Error(t, err)
		})
	}
}

func TestFractionalTimeoutsApply(t *testing.T) {
	start := time.Now()
	o := run(t, "[a]\nck cmd(\"sleep 30\", {timeout 0.3})")
	assert.False(t, o.results[0].Stmts[0].Pass)
	assert.Contains(t, o.results[0].Stmts[0].Detail, "timed out after 300ms")
	assert.Less(t, time.Since(start), 3*time.Second)

	// [DEFAULTS] supplies it, and the call's own value beats it
	start = time.Now()
	o = run(t, "[DEFAULTS] {timeout 0.4}\n[a]\nck cmd(\"sleep 30\")")
	assert.Contains(t, o.results[0].Stmts[0].Detail, "timed out after 400ms")
	assert.Less(t, time.Since(start), 3*time.Second)
	o = run(t, "[DEFAULTS] {timeout 5}\n[a]\nck cmd(\"sleep 30\", {timeout 0.2})")
	assert.Contains(t, o.results[0].Stmts[0].Detail, "timed out after 200ms")
}
