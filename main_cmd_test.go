package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCmdInAConfig(t *testing.T) {
	o := run(t, `[a]
ck cmd("echo hello; echo oops >&2; exit 2", exitstatus == 2, stdout == "hello", stderr =~ /^oops$/)
ck cmd("true", exitstatus == 0, stdout == "", stderr == "")
ck cmd("exit 1", exitstatus == 0)
ck ! cmd("exit 1", exitstatus == 0)
ck cmd("echo 42", stdout > "4", exitstatus != 1)
if cmd("test -d /", exitstatus == 0)
`)
	got := map[string]StmtResult{}
	for _, st := range o.results[0].Stmts {
		got[st.Text] = st
	}
	assert.Len(t, o.results[0].Stmts, 6)
	failed := 0
	for _, st := range o.results[0].Stmts {
		if !st.Pass {
			failed++
			assert.Equal(t, `cmd("exit 1", exitstatus == 0)`, st.Text)
			assert.Contains(t, st.Detail, "exitstatus == 0 is false (got 1)")
		}
	}
	assert.Equal(t, 1, failed)
}

func TestCmdOptionsAndMsg(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "seen")
	line := fmt.Sprintf(`if [ -e %[1]s ]; then echo ok; else touch %[1]s; sleep 30; fi`, marker)

	start := time.Now()
	o := run(t, fmt.Sprintf("[a]\nck cmd(%q, {timeout 1, retry 1, sleep 0.1}, stdout == \"ok\")", line))
	assert.True(t, o.results[0].Stmts[0].Pass, o.out)
	assert.Less(t, time.Since(start), 10*time.Second)

	// [DEFAULTS] timeout applies, but [DEFAULTS] retry does not: a command is run
	// again only when the call itself says so
	require.NoError(t, os.Remove(marker))
	o = run(t, fmt.Sprintf("[DEFAULTS] {timeout 1, retry 1}\n[a]\nck cmd(%q, stdout == \"ok\")", line))
	assert.False(t, o.results[0].Stmts[0].Pass)
	assert.Contains(t, o.results[0].Stmts[0].Detail, "timed out")
	require.NoError(t, os.Remove(marker))
	o = run(t, fmt.Sprintf("[DEFAULTS] {timeout 1}\n[a]\nck cmd(%q, {retry 1}, stdout == \"ok\")", line))
	assert.True(t, o.results[0].Stmts[0].Pass, o.out)

	// msg replaces the built-in text, for a failed test and for a hang alike
	o = run(t, `[a]
ck cmd("exit 4", {msg "service check failed"}, exitstatus == 0)
ck cmd("sleep 30", {timeout 1, msg "it hung"})`)
	for i, want := range []string{"service check failed", "it hung"} {
		assert.False(t, o.results[0].Stmts[i].Pass)
		assert.Equal(t, want, o.results[0].Stmts[i].Detail)
	}
}

// A non-zero exit is retried (when the call has a retry); a zero exit is not.
func TestCmdRetriesNonZeroOnly(t *testing.T) {
	dir := t.TempDir()
	count := func(name string) int {
		raw, _ := os.ReadFile(filepath.Join(dir, name))
		return strings.Count(string(raw), "x")
	}
	script := func(name, tail string) string {
		return fmt.Sprintf("echo x >> %s; %s", filepath.Join(dir, name), tail)
	}
	// fails twice, then works: the third run passes the checks
	o := run(t, fmt.Sprintf("[a]\nck cmd(%q, {retry=2}, exitstatus == 0)", script("a", fmt.Sprintf("[ $(wc -l < %s) -ge 3 ]", filepath.Join(dir, "a")))))
	assert.True(t, o.results[0].Stmts[0].Pass, o.out)
	assert.Equal(t, 3, count("a"))
	// zero is never repeated
	run(t, fmt.Sprintf("[a]\nck cmd(%q, {retry=5})", script("b", "exit 0")))
	assert.Equal(t, 1, count("b"))
	// no retry in the call: one run, even when [DEFAULTS] has one
	run(t, fmt.Sprintf("[DEFAULTS] {retry 4}\n[a]\nck cmd(%q, exitstatus == 1)", script("c", "exit 1")))
	assert.Equal(t, 1, count("c"))
	// still failing after the retries: 1 + 2 runs, and the last status is tested
	o = run(t, fmt.Sprintf("[a]\nck cmd(%q, {retry=2}, exitstatus == 0)\nck cmd(%q, {retry=2}, exitstatus == 7)", script("d", "exit 7"), script("e", "exit 7")))
	assert.Equal(t, 3, count("d"))
	assert.Equal(t, 3, count("e"))
	for _, st := range o.results[0].Stmts {
		if strings.Contains(st.Text, "exitstatus == 0") {
			assert.False(t, st.Pass)
			assert.Contains(t, st.Detail, "got 7")
		} else {
			assert.True(t, st.Pass, st.Detail)
		}
	}
	// "exitstatus != 0" works too: after the retries the last status is what is tested
	run(t, fmt.Sprintf("[a]\nck cmd(%q, {retry=1}, exitstatus != 0)", script("g", "exit 5")))
	assert.Equal(t, 2, count("g"))
	o = run(t, fmt.Sprintf("[a]\nck cmd(%q, exitstatus != 0)\nck cmd(\"true\", exitstatus != 0)", script("h", "exit 5")))
	assert.True(t, o.results[0].Stmts[0].Pass, o.out)
	assert.False(t, o.results[0].Stmts[1].Pass)
	// other functions still use [DEFAULTS] retry (here it only has to be accepted)
	mustLoad(t, "[DEFAULTS] {retry 2}\n[a]\nck lstat(\"/\", type == d)")
}

func TestCmdVariablesAndChains(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "d"), 0o755))
	allPass(t, fmt.Sprintf(`[a]
r = cmd("echo %s/d; echo warn >&2")
ck r.exitstatus == 0
ck r.stderr == "warn"
ck lstat(r.stdout, type == d)
s = sed(r.stdout, "s,.*/,,")
ck s.value == "d"
c = cmd("exit 0")
n = sed(c.exitstatus, "s/0/zero/")
ck n.value == "zero"
ck cmd("test %[1]s/d = $(echo %[1]s/d)", exitstatus == 0)
`, dir, dir))
	// a command that hangs does not stop the rest of the section's work from being reported
	o := run(t, `[a]
ck cmd("sleep 30", {timeout 1})
ck cmd("echo after", stdout == "after")`)
	assert.False(t, o.results[0].Stmts[0].Pass)
	assert.True(t, o.results[0].Stmts[1].Pass)
}

// A command has effects, so it must not run ahead of an earlier `if`.
func TestCmdRunsWhenItsStatementIsReached(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "ran")
	// a failing guard skips the section's checks, so the command never runs...
	run(t, fmt.Sprintf("[a]\nif ! lstat(\"/\", type == d)\nck cmd(\"touch %s\")\n", out))
	_, err := os.Stat(out)
	assert.True(t, os.IsNotExist(err), "the guard failed, so the command must not have run")
	// ...and a passing one lets it run
	run(t, fmt.Sprintf("[a]\nif lstat(\"/\", type == d)\nck cmd(\"touch %s\")\n", out))
	_, err = os.Stat(out)
	assert.NoError(t, err)

	// sections are independent and concurrent: both hang for 1s, together well under 2s
	start := time.Now()
	run(t, "[a]\nck cmd(\"sleep 30\", {timeout 1})\n[b]\nck cmd(\"sleep 30\", {timeout 1})")
	assert.Less(t, time.Since(start), 1900*time.Millisecond)
}

func TestCmdConfigErrors(t *testing.T) {
	for name, c := range map[string]struct{ cfg, want string }{
		"unknown item":      {`[a]` + "\n" + `ck cmd("true", status == 0)`, `has no test "status"`},
		"text for a number": {`[a]` + "\n" + `ck cmd("true", exitstatus == "0")`, "expected a number"},
		"regex on number":   {`[a]` + "\n" + `ck cmd("true", exitstatus =~ /0/)`, "not allowed"},
		"unknown option":    {`[a]` + "\n" + `ck cmd("true", {nope 1})`, `no option "nope"`},
		"two commands":      {`[a]` + "\n" + `ck cmd("true", "false")`, "takes only one string"},
		"test in assign":    {`[a]` + "\n" + `v = cmd("true", exitstatus == 0)`, "would gate nothing"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := load(c.cfg)
			require.Error(t, err)
			assert.ErrorContains(t, err, c.want)
		})
	}
	assert.True(t, strings.Contains(run(t, `[a]`+"\n"+`ck cmd("")`).results[0].Stmts[0].Detail, "the command is empty"))
}

// $? is the shell's name for the exit status, an alias of exitstatus
func TestCmdDollarQuestionIsExitstatus(t *testing.T) {
	o := run(t, `[a]
ck cmd("exit 3", $? == 3)
ck cmd("exit 3", $? != 0, exitstatus == 3)
ck cmd("true", $? == 0)
ck ! cmd("exit 3", $? == 0)
r = cmd("exit 4")
ck r.$? == 4
ck r.exitstatus == 4
ck cmd("exit 1", $? == 0)
`)
	failed := 0
	for _, st := range o.results[0].Stmts {
		if !st.Pass {
			failed++
			assert.Contains(t, st.Detail, "exitstatus == 0 is false (got 1)")
		}
	}
	assert.Equal(t, 1, failed, o.out)
	s := run(t, `[a]`+"\n"+`r = cmd("exit 5")`+"\n"+`n = sed(r.$?, "s/5/five/")`+"\n"+`ck n.value == "five"`).results[0].Stmts
	assert.True(t, s[2].Pass, s[2].Detail)
	// a function without the item says so, and $? is no use as a name
	_, _, err := load(`[a]` + "\n" + `ck lstat("/", $? == 0)`)
	assert.ErrorContains(t, err, "no test")
	assert.True(t, strings.Contains(run(t, `[a]`+"\n"+`ck cmd("echo '$?'", stdout == "$?")`).out, "PASS"))
}
