package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A script that times out must not leave the processes it started behind.
func TestTimedOutScriptLeavesNoChildProcess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	run(t, fmt.Sprintf(`[ACTION_SCRIPTS]
as:hang = "sleep 60 & echo $! > %s; wait"
[a]
%s
action as:hang {timeout 1}
`, pidFile, failing))

	b, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	require.NoError(t, err)
	// Give the kill a moment to land, then ask whether the child still exists.
	ok := assert.Eventually(t, func() bool { return gone(pid) }, 2*time.Second, 50*time.Millisecond,
		"child process %d is still running after the script timed out", pid)
	if !ok {
		syscall.Kill(pid, syscall.SIGKILL) // do not leave it behind if the test fails
	}
}

// gone reports whether the process no longer runs. A killed process whose
// parent is gone stays a zombie until init reaps it (some containers never do),
// and signal 0 still succeeds on a zombie, so on Linux the state is read too.
func gone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return true
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false // no /proc to ask, and the process answered signal 0
	}
	// "pid (name) S ...": the state letter follows the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] == 'Z'
}
