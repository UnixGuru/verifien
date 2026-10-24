package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawReport runs cfg (which ends with `action as:rec`) and returns the JSON the
// script received, decoded loosely so the field names themselves are tested.
func rawReport(t *testing.T, cfgBody string) map[string]any {
	t.Helper()
	out := filepath.Join(t.TempDir(), "stdin.json")
	run(t, fmt.Sprintf("[ACTION_SCRIPTS]\nas:rec = \"cat > %s\"\n%s\naction as:rec\n", out, cfgBody))
	var rep map[string]any
	require.NoError(t, json.Unmarshal([]byte(readFile(t, out)), &rep))
	return rep
}

func TestJSONFailuresHaveLineAndAllOptions(t *testing.T) {
	rep := rawReport(t, `[web]
# a comment

ck cmd("sleep 5", {timeout = 0.2}, exitstatus == 0)
ck environ("VERIFIEN_SURELY_UNSET", {msg "gone", retry 2, sleep 0.5}, set == yes)
ck lstat("/nonexistent/x", type == f)
v = environ("PATH")
ck v.set == no`)
	fails := rep["failures"].([]any)
	require.Len(t, fails, 4)

	f := func(i int) map[string]any { return fails[i].(map[string]any) }
	// the line is the line of the ck in the file (rawReport puts two lines of [ACTION_SCRIPTS] first)
	wantLine := []float64{6, 7, 8, 10}
	for i, want := range wantLine {
		assert.Equal(t, want, f(i)["line"], "failure %d: %v", i, f(i)["ck"])
		assert.Equal(t, "test.cfg", f(i)["file"])
	}
	// every option, whether the config gave it or not
	assert.Equal(t, map[string]any{"timeout": 0.2, "retry": 0.0, "sleep": 0.0}, f(0)["options"], "cmd: its own timeout, no default retry")
	assert.Equal(t, map[string]any{"timeout": 10.0, "retry": 2.0, "sleep": 0.5, "msg": "gone"}, f(1)["options"])
	assert.Equal(t, map[string]any{"timeout": 10.0, "retry": 0.0, "sleep": 0.0}, f(2)["options"], "nothing given: the built-in values")
	// a check on a variable calls no function, so it has no options
	assert.NotContains(t, f(3), "options")
}

func TestJSONOptionsComeFromDefaults(t *testing.T) {
	rep := rawReport(t, `[DEFAULTS] {timeout 3.5, retry 4, sleep 0.25}
[a]
ck lstat("/nonexistent/x", type == f)
ck cmd("exit 1", {retry 1}, exitstatus == 0)
ck lstat("/nonexistent/y", {timeout 1}, type == f)`)
	fails := rep["failures"].([]any)
	require.Len(t, fails, 3)
	opts := func(i int) any { return fails[i].(map[string]any)["options"] }
	assert.Equal(t, map[string]any{"timeout": 3.5, "retry": 4.0, "sleep": 0.25}, opts(0), "all from [DEFAULTS]")
	assert.Equal(t, map[string]any{"timeout": 3.5, "retry": 1.0, "sleep": 0.25}, opts(1), "cmd: retry only from the call; the rest from [DEFAULTS]")
	assert.Equal(t, map[string]any{"timeout": 1.0, "retry": 4.0, "sleep": 0.25}, opts(2), "the call's timeout wins")
}

func TestJSONLineInAnIncludedFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "inc.cfg"), []byte("[web]\n\nck lstat(\"/nonexistent/inc\", type == f)\naction as:rec\n"), 0o644))
	out := filepath.Join(dir, "stdin.json")
	main := fmt.Sprintf("[ACTION_SCRIPTS]\nas:rec = \"cat > %s\"\ninclude \"inc.cfg\"\n", out)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.cfg"), []byte(main), 0o644))
	t.Chdir(dir)
	f, set, err := parseAndValidate(buildParser(), "main.cfg", main)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAll(ctx, f, set, cancel)
	var rep map[string]any
	require.NoError(t, json.Unmarshal([]byte(readFile(t, out)), &rep))
	fail := rep["failures"].([]any)[0].(map[string]any)
	assert.Equal(t, "inc.cfg", fail["file"], "the file the ck is in, not the including file")
	assert.EqualValues(t, 3, fail["line"])
}

func TestDebugShowsLineAndOptions(t *testing.T) {
	o := run(t, `[a]
v = lstat("/")

ck cmd("sleep 5", {timeout = 0.2}, exitstatus == 0)
ck environ("PATH", {msg "m", retry 1}, set == yes)
ck v.type == d
if ! lstat("/", type == f)
`)
	lines := map[string]string{}
	for _, l := range strings.Split(o.out, "\n") {
		if i := strings.Index(l, "test.cfg:"); i >= 0 {
			lines[strings.SplitN(l[i:], "  ", 2)[0]] = l
		}
	}
	// every statement line carries file:line
	for _, n := range []string{"2", "4", "5", "6", "7"} {
		assert.Contains(t, lines, "test.cfg:"+n, o.out)
	}
	assert.Contains(t, lines["test.cfg:4"], `cmd("sleep 5", exitstatus == 0)  {timeout 0.2, retry 0, sleep 0}`)
	assert.Contains(t, lines["test.cfg:5"], `{timeout 10, retry 1, sleep 0, msg "m"}`)
	assert.Contains(t, lines["test.cfg:2"], `{timeout 10, retry 0, sleep 0}`, "an assignment shows them too")
	assert.Contains(t, lines["test.cfg:7"], `{timeout 10, retry 0, sleep 0}`, "so does an if")
	assert.NotContains(t, lines["test.cfg:6"], "{timeout", "a check on a variable runs no function")
	// the order: status, keyword, position, text, options, detail
	assert.Regexp(t, `FAIL  ck  test.cfg:4  cmd\(.*\)  \{timeout 0.2, retry 0, sleep 0\}  -> `, lines["test.cfg:4"])
}
