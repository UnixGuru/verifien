package shell

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"verifien/spec"
)

func call(t *testing.T, line string, mod ...func(*spec.Request)) (spec.Result, error) {
	t.Helper()
	fs, ok := spec.Lookup("cmd")
	require.True(t, ok)
	req := spec.Request{Target: line, Timeout: 5 * time.Second}
	for _, m := range mod {
		m(&req)
	}
	return fs.Run(context.Background(), req)
}

func TestOutputAndStatus(t *testing.T) {
	res, err := call(t, `echo out; echo err >&2; exit 3`)
	require.NoError(t, err)
	assert.EqualValues(t, 3, res["exitstatus"])
	assert.Equal(t, "out", res["stdout"], "the trailing newline is dropped")
	assert.Equal(t, "err", res["stderr"])

	res, err = call(t, `printf 'a\n\nb\n\n'`)
	require.NoError(t, err)
	assert.Equal(t, "a\n\nb", res["stdout"], "only trailing newlines go")

	res, err = call(t, `true`)
	require.NoError(t, err)
	assert.EqualValues(t, 0, res["exitstatus"])
	assert.Equal(t, "", res["stdout"])
}

func TestShellFeaturesAndStdin(t *testing.T) {
	res, err := call(t, `echo $((6*7)) | tr 4 5 && cat`) // cat sees /dev/null, so does not hang
	require.NoError(t, err)
	assert.Equal(t, "52", res["stdout"])
	res, err = call(t, `definitely-not-a-command`)
	require.NoError(t, err)
	assert.EqualValues(t, 127, res["exitstatus"])
	assert.Contains(t, res["stderr"], "not found")
}

func TestEmptyCommand(t *testing.T) {
	for _, line := range []string{"", "   "} {
		_, err := call(t, line)
		assert.ErrorContains(t, err, "the command is empty")
	}
}

func TestTimeoutKillsTheWholeGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	_, err := call(t, `sleep 30 & echo $! > `+pidfile+`; wait`, func(r *spec.Request) { r.Timeout = 300 * time.Millisecond })
	assert.ErrorContains(t, err, "timed out")
	assert.Less(t, time.Since(start), 5*time.Second)
	// the sleep the command started is gone too
	raw, rerr := os.ReadFile(pidfile)
	require.NoError(t, rerr)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.Positive(t, pid)
	assert.Eventually(t, func() bool { return !alive(pid) }, 3*time.Second, 50*time.Millisecond, "child %d still running", pid)
}

func TestRetryAfterATimeoutAndSleep(t *testing.T) {
	// the first attempt hangs, the second finds the marker and answers at once
	marker := filepath.Join(t.TempDir(), "seen")
	line := `if [ -e ` + marker + ` ]; then echo second; else touch ` + marker + `; sleep 30; fi`
	start := time.Now()
	res, err := call(t, line, func(r *spec.Request) {
		r.Timeout, r.Retry, r.Sleep = 300*time.Millisecond, 1, 200*time.Millisecond
	})
	require.NoError(t, err)
	assert.Equal(t, "second", res["stdout"])
	assert.GreaterOrEqual(t, time.Since(start), 500*time.Millisecond, "timeout plus sleep")

	// without a retry the hang is the result
	_ = os.Remove(marker)
	_, err = call(t, line, func(r *spec.Request) { r.Timeout = 300 * time.Millisecond })
	assert.ErrorContains(t, err, "timed out")
}

func runs(t *testing.T, path string) int {
	t.Helper()
	raw, _ := os.ReadFile(path)
	return strings.Count(string(raw), "x")
}

func TestNonZeroExitIsRetriedAndZeroIsNot(t *testing.T) {
	dir := t.TempDir()
	// a non-zero status is retried; when the retries run out it is still a result
	n := filepath.Join(dir, "n")
	res, err := call(t, `echo x >> `+n+`; exit 1`, func(r *spec.Request) { r.Retry = 3 })
	require.NoError(t, err)
	assert.EqualValues(t, 1, res["exitstatus"])
	assert.Equal(t, 4, runs(t, n), "the first run and 3 retries")

	// zero is done at once, whatever the retry
	z := filepath.Join(dir, "z")
	res, err = call(t, `echo x >> `+z+`; exit 0`, func(r *spec.Request) { r.Retry = 3 })
	require.NoError(t, err)
	assert.EqualValues(t, 0, res["exitstatus"])
	assert.Equal(t, 1, runs(t, z))

	// it stops retrying as soon as an attempt exits 0
	f := filepath.Join(dir, "f")
	res, err = call(t, `echo x >> `+f+`; [ $(wc -l < `+f+`) -ge 3 ]`, func(r *spec.Request) { r.Retry = 5 })
	require.NoError(t, err)
	assert.EqualValues(t, 0, res["exitstatus"])
	assert.Equal(t, 3, runs(t, f))

	// no retry asked: one run
	o := filepath.Join(dir, "o")
	res, err = call(t, `echo x >> `+o+`; exit 2`)
	require.NoError(t, err)
	assert.EqualValues(t, 2, res["exitstatus"])
	assert.Equal(t, 1, runs(t, o))
}

func TestRetryWaitsSleepBetweenNonZeroRuns(t *testing.T) {
	start := time.Now()
	_, err := call(t, `exit 1`, func(r *spec.Request) { r.Retry, r.Sleep = 2, 200*time.Millisecond })
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 400*time.Millisecond, "two pauses")
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestTheLastFailureDecides(t *testing.T) {
	// non-zero first, then a hang: the result is the timeout, not the old exit status
	marker := filepath.Join(t.TempDir(), "seen")
	line := `if [ -e ` + marker + ` ]; then sleep 30; else touch ` + marker + `; exit 1; fi`
	_, err := call(t, line, func(r *spec.Request) { r.Retry, r.Timeout = 1, 300*time.Millisecond })
	assert.ErrorContains(t, err, "timed out")
}

func TestKilledBySignalIsAnError(t *testing.T) {
	_, err := call(t, `kill -9 $$`)
	assert.ErrorContains(t, err, "signal")
}

func TestHugeOutputIsCappedAndDoesNotBlock(t *testing.T) {
	res, err := call(t, `head -c 3000000 /dev/zero | tr '\0' x; echo done >&2`)
	require.NoError(t, err)
	assert.Len(t, res["stdout"], maxOutput)
	assert.Equal(t, "done", res["stderr"])
}

func TestBackgroundChildHoldingThePipeDoesNotHangUs(t *testing.T) {
	start := time.Now()
	res, err := call(t, `echo hi; (sleep 5 &) ; exit 0`)
	require.NoError(t, err)
	assert.Equal(t, "hi", res["stdout"])
	assert.Less(t, time.Since(start), 10*time.Second)
}
