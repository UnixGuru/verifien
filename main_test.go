package main

// End-to-end tests: real config text goes through the real parser, validator and
// runner, with real files, processes, HTTP servers and a UDP syslog listener.
//
// A few test-only functions are registered here (counter, blocker, hang,
// slowsecond) because the real functions cannot be made to count calls or
// block on demand.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"verifien/spec"
)

// ---------------------------------------------------------------------------
// Test-only functions
// ---------------------------------------------------------------------------

var (
	countsMu sync.Mutex
	counts   = map[string]int{}
)

// bump adds one to the named counter and returns the new value.
func bump(key string) int {
	countsMu.Lock()
	defer countsMu.Unlock()
	counts[key]++
	return counts[key]
}

func count(key string) int {
	countsMu.Lock()
	defer countsMu.Unlock()
	return counts[key]
}

func init() {
	yesNo := spec.Field{Kind: spec.KEnum, Enum: []string{"yes", "no"}}

	// counter("key"): n is how many times it has been called, so a ck such as
	// n >= 3 fails twice and then passes, which is what rerun needs.
	spec.Register("counter", spec.FuncSpec{
		Run: func(_ context.Context, req spec.Request) (spec.Result, error) {
			return spec.Result{"n": int64(bump(req.Target))}, nil
		},
		Options: spec.StdOptions(),
		Tests:   map[string]spec.Field{"n": {Kind: spec.KInt}},
	})

	// blocker("key:ms"): every attempt counts under "key" and sleeps ms
	// milliseconds. A negative ms fails at once instead. It goes through
	// spec.Attempt, so it shows how timeout, retry and sleep are resolved.
	spec.Register("blocker", spec.FuncSpec{
		Run: func(ctx context.Context, req spec.Request) (spec.Result, error) {
			key, msText, _ := strings.Cut(req.Target, ":")
			ms, _ := strconv.Atoi(msText)
			return spec.Attempt(ctx, req, func() (spec.Result, error) {
				bump(key)
				if ms < 0 {
					return nil, fmt.Errorf("blocker failed")
				}
				time.Sleep(time.Duration(ms) * time.Millisecond)
				return spec.Result{"ok": "yes"}, nil
			})
		},
		Options: spec.StdOptions(),
		Tests:   map[string]spec.Field{"ok": yesNo},
	})

	// hang(""): blocks until the run is cancelled.
	spec.Register("hang", spec.FuncSpec{
		Run: func(ctx context.Context, req spec.Request) (spec.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		Options: spec.StdOptions(),
		Tests:   map[string]spec.Field{"ok": yesNo},
	})

	// slowsecond("key"): the first call answers n = 0 at once; every later call
	// blocks until its context ends. It makes a pass that only hangs on a rerun.
	spec.Register("slowsecond", spec.FuncSpec{
		Run: func(ctx context.Context, req spec.Request) (spec.Result, error) {
			if bump(req.Target) == 1 {
				return spec.Result{"n": int64(0)}, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
		Options: spec.StdOptions(),
		Tests:   map[string]spec.Field{"n": {Kind: spec.KInt}},
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func load(cfg string) (*File, Settings, error) {
	return parseAndValidate(buildParser(), "test.cfg", cfg)
}

func mustLoad(t *testing.T, cfg string) (*File, Settings) {
	t.Helper()
	f, set, err := load(cfg)
	require.NoError(t, err, "config rejected:\n%s", cfg)
	return f, set
}

type outcome struct {
	results []SectionResult
	set     Settings
	out     string // what the report printed
	code    int    // the exit status
	took    time.Duration
}

// run does what main does: parse, validate, run every section, render.
func run(t *testing.T, cfg string) outcome {
	t.Helper()
	f, set := mustLoad(t, cfg)
	ctx, stopAll := context.WithCancel(context.Background())
	defer stopAll()
	start := time.Now()
	results := runAll(ctx, f, set, stopAll)
	took := time.Since(start)
	var buf bytes.Buffer
	code := render(&buf, results, set)
	return outcome{results, set, buf.String(), code, took}
}

func contains(t *testing.T, out, want string) {
	t.Helper()
	assert.Contains(t, out, want)
}

func notContains(t *testing.T, out, want string) {
	t.Helper()
	assert.NotContains(t, out, want)
}

func outcomeNames(sr SectionResult) []string {
	var names []string
	for _, a := range sr.Actions {
		names = append(names, a.Name)
	}
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// A ck that always fails, and one that always passes.
const (
	failing = `ck environ("VERIFIEN_SURELY_UNSET", set == yes)`
	passing = `ck environ("PATH", set == yes)`
)

// ---------------------------------------------------------------------------
// Config validation
// ---------------------------------------------------------------------------

func TestConfigErrors(t *testing.T) {
	cases := []struct{ name, cfg, want string }{
		{"unknown action", "[a]\n" + failing + "\naction boom", `unknown action "boom"`},
		{"unknown action lists the known ones", "[a]\n" + failing + "\naction boom", "exitnow, rerun, as:IDENT"},
		{"undefined script", "[a]\n" + failing + "\naction as:nope", "as:nope is not defined in [ACTION_SCRIPTS]"},
		{"bare as", "[a]\n" + failing + "\naction as", "write as:IDENT"},
		{"exit takes no options", "[a]\n" + failing + "\naction exit {retry 2}", "takes no options"},
		{"exitnow takes no options", "[a]\n" + failing + "\naction exitnow {timeout 2}", "takes no options"},
		{"webhook needs a url", "[a]\n" + failing + "\naction webhook", "no url"},
		{"webhook url must be http", "[a]\n" + failing + "\naction webhook {url \"ftp://x\"}", "http:// or https://"},
		{"webhook unknown option", "[a]\n" + failing + "\naction webhook {url \"http://x\", bogus 1}", `no option "bogus"`},
		{"logfile needs a path", "[a]\n" + failing + "\naction logfile", "no path"},
		{"syslog bad address", "[DEFAULTS] {syslog_address = \"host:514\"}\n[a]\n" + failing + "\naction syslog", "udp://host:514"},
		{"syslog bad facility", "[DEFAULTS] {syslog_facility = nope}\n[a]\n" + failing, "must be one of"},
		{"duplicate action", "[a]\n" + failing + "\naction syslog, syslog", "already given"},
		{"duplicate across lines", "[a]\n" + failing + "\naction exit\naction exit", "already given"},
		{"action without any ck", "[a]\nif environ(\"PATH\", set == yes)\naction exit", "can never run"},
		{"script defined twice", "[ACTION_SCRIPTS]\nas:x = \"true\"\nas:x = \"false\"\n[a]\n" + failing, "defined twice"},
		{"empty script", "[ACTION_SCRIPTS]\nas:x = \"  \"\n[a]\n" + failing, "empty script"},
		{"two script blocks", "[ACTION_SCRIPTS]\nas:x = \"true\"\n[action_scripts]\nas:y = \"true\"\n[a]\n" + failing, "appears twice"},
		{"rerun timeout must be positive", "[a]\n" + failing + "\naction rerun {timeout 0}", "greater than 0"},
		{"rerun_timeout must be positive", "[DEFAULTS] {rerun_timeout = 0}\n[a]\n" + failing, "greater than 0"},
		{"unknown default", "[DEFAULTS] {bogus = 1}\n[a]\n" + failing, `unknown setting "bogus"`},
		{"exit status range", "[DEFAULTS] {exit_status_bad = 300}\n[a]\n" + failing, "between 0 and 255"},
		{"exit status given twice", "[DEFAULTS] {exit_status_bad = 2, exit_status_bad = 3}\n[a]\n" + failing, "given twice"},
		{"sleep must be a number", "[a]\nck stat(\"/\", {sleep \"x\"}, type == d)", "expected a number"},
		{"sleep in DEFAULTS", "[DEFAULTS] {sleep = 1s}\n[a]\n" + failing, "invalid number"},
		{"environ ignores nothing it cannot parse", "[a]\nck environ(\"X\", {retry \"x\"}, set == yes)", "expected a number"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := load(c.cfg)
			assert.ErrorContains(t, err, c.want)
		})
	}
}

func TestValidConfigs(t *testing.T) {
	for name, cfg := range map[string]string{
		"action lists and comments": "[ACTION_SCRIPTS]\nas:x = \"true\"  # note\n[a]\n" + failing + "\naction as:x # c\naction exit",
		"options on actions":        "[a]\n" + failing + "\naction rerun {retry 3, sleep 0.5, timeout 9}, exit",
		"environ takes std options": "[a]\nck environ(\"X\", {timeout 1, retry 2, sleep 0.5, msg \"m\"}, set == yes)",
		"all defaults":              "[DEFAULTS] {timeout = 3, retries = 2, sleep = 0.5, rerun_retry = 2, rerun_sleep = 1.5, rerun_timeout = 30, exit_status_good = 0, exit_status_bad = 2, exit_status_now = 3}\n[a]\n" + passing,
	} {
		t.Run(name, func(t *testing.T) { mustLoad(t, cfg) })
	}
}

// ---------------------------------------------------------------------------
// if, ck, msg
// ---------------------------------------------------------------------------

func TestIfGatesCksAndActions(t *testing.T) {
	o := run(t, "[a]\nif environ(\"VERIFIEN_SURELY_UNSET\", set == yes)\n"+failing+"\naction exitnow")
	r := o.results[0]
	assert.True(t, r.Skipped)
	assert.False(t, r.Failed)
	assert.Empty(t, r.Actions)
	assert.Equal(t, 0, o.code)
	contains(t, o.out, "SKIP")
}

func TestAllIfsMustPassBeforeCksRun(t *testing.T) {
	o := run(t, "[a]\nif environ(\"PATH\", set == yes)\nif environ(\"VERIFIEN_SURELY_UNSET\", set == yes)\nif environ(\"PATH\", set == yes)\n"+failing+"\naction exit")
	assert.True(t, o.results[0].Skipped, "one failing if must skip every ck")
	assert.Equal(t, 0, o.code)
}

func TestMsgReplacesTheBuiltInMessage(t *testing.T) {
	o := run(t, "[a]\nck environ(\"VERIFIEN_SURELY_UNSET\", {msg \"must be exported\"}, set == yes)")
	contains(t, o.out, "-> must be exported")
	notContains(t, o.out, "is false")
}

// ---------------------------------------------------------------------------
// Exit status
// ---------------------------------------------------------------------------

func TestExitStatus(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want int
	}{
		{"no failures", "[a]\n" + passing, 0},
		{"failure but no action", "[a]\n" + failing, 0},
		{"exit action, nothing failed", "[a]\n" + passing + "\naction exit", 0},
		{"exit", "[a]\n" + failing + "\naction exit", 1},
		{"exitnow", "[a]\n" + failing + "\naction exitnow", 1},
		{"good override", "[DEFAULTS] {exit_status_good = 7}\n[a]\n" + passing, 7},
		{"good override with a plain failure", "[DEFAULTS] {exit_status_good = 7}\n[a]\n" + failing, 7},
		{"bad override", "[DEFAULTS] {exit_status_bad = 20}\n[a]\n" + failing + "\naction exit", 20},
		{"now override", "[DEFAULTS] {exit_status_now = 30}\n[a]\n" + failing + "\naction exitnow", 30},
		{"bad override does not touch exitnow", "[DEFAULTS] {exit_status_bad = 20}\n[a]\n" + failing + "\naction exitnow", 1},
		{"exitnow wins over exit", "[DEFAULTS] {exit_status_bad = 20, exit_status_now = 30}\n[a]\n" + failing + "\naction exit\n[b]\n" + failing + "\naction exitnow", 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := run(t, c.cfg)
			assert.Equal(t, c.want, o.code, o.out)
		})
	}
}

func TestExitDoesNotCancelOtherSections(t *testing.T) {
	o := run(t, "[a]\n"+failing+"\naction exit\n[b]\nck blocker(\"exit-other:400\", ok == yes)")
	assert.False(t, o.results[1].Aborted, "exit must let every ck run")
	assert.False(t, o.results[1].Failed)
	assert.Equal(t, 1, o.code)
}

func TestExitnowCancelsOtherSections(t *testing.T) {
	o := run(t, "[DEFAULTS] {exit_status_now = 9}\n[fast]\n"+failing+"\naction exitnow\n[stuck]\nck hang(\"\", ok == yes)")
	assert.True(t, o.results[1].Aborted, "the other section should be aborted")
	assert.Less(t, o.took, 2*time.Second, "exitnow must not wait for the other section")
	assert.Equal(t, 9, o.code)
	contains(t, o.out, "ABORT")
}

func TestExitnowStopsAtTheFirstFailedCk(t *testing.T) {
	key := "exitnow-first"
	o := run(t, "[a]\n"+failing+"\nck counter(\""+key+"\", n >= 1)\naction exitnow")
	assert.Equal(t, 0, count(key), "the second ck ran; exitnow stops at the first failure")
	assert.Len(t, o.results[0].Stmts, 1)
}

func TestExitRunsEveryCkOfItsSection(t *testing.T) {
	key := "exit-all"
	run(t, "[a]\n"+failing+"\nck counter(\""+key+"\", n >= 1)\naction exit")
	assert.Equal(t, 1, count(key), "exit must run every ck")
}

// ---------------------------------------------------------------------------
// timeout, retry, sleep for functions: option > [DEFAULTS] > compiled-in
// ---------------------------------------------------------------------------

func TestFunctionRetryAndSleepFromDefaults(t *testing.T) {
	o := run(t, "[DEFAULTS] {retry = 2, sleep = 0.3}\n[a]\nck blocker(\"fn-defaults:-1\", ok == yes)")
	assert.Equal(t, 3, count("fn-defaults"), "1 + retry 2")
	assert.GreaterOrEqual(t, o.took, 550*time.Millisecond, "two sleeps of 0.3s")
}

func TestFunctionOptionBeatsDefaults(t *testing.T) {
	run(t, "[DEFAULTS] {retry = 4, sleep = 2}\n[a]\nck blocker(\"fn-option:-1\", {retry 1, sleep 0.1}, ok == yes)")
	assert.Equal(t, 2, count("fn-option"), "the call's retry 1 beats DEFAULTS retry 4")
}

func TestFunctionCompiledInDefaultIsOneAttempt(t *testing.T) {
	run(t, "[a]\nck blocker(\"fn-compiled:-1\", ok == yes)")
	assert.Equal(t, 1, count("fn-compiled"))
}

func TestFunctionTimeoutIsPerAttempt(t *testing.T) {
	o := run(t, "[a]\nck blocker(\"fn-timeout:5000\", {timeout 1, retry 1}, ok == yes)")
	assert.Equal(t, 2, count("fn-timeout"))
	assert.GreaterOrEqual(t, o.took, 1900*time.Millisecond, "two 1s attempts")
	assert.LessOrEqual(t, o.took, 3*time.Second)
	contains(t, o.out, "timed out after 1s")
}

// ---------------------------------------------------------------------------
// rerun
// ---------------------------------------------------------------------------

func TestRerunPassesOnALaterPassAndNothingFires(t *testing.T) {
	logfile := filepath.Join(t.TempDir(), "log")
	o := run(t, fmt.Sprintf("[a]\nck counter(\"rerun-late\", n >= 3)\naction rerun {retry 5}, logfile {path %q}, exit", logfile))
	r := o.results[0]
	assert.False(t, r.Failed)
	assert.Equal(t, 2, r.Reruns)
	assert.Equal(t, 5, r.RerunMax)
	assert.Empty(t, r.Actions)
	assert.Equal(t, 0, o.code)
	assert.NoFileExists(t, logfile, "the logfile action ran although the section passed on a rerun")
	contains(t, o.out, "RERUN  passed on rerun 2 of 5")
}

func TestRerunGivesUpThenActionsRunOnce(t *testing.T) {
	logfile := filepath.Join(t.TempDir(), "log")
	o := run(t, fmt.Sprintf("[a]\nck counter(\"rerun-never\", n >= 100)\naction rerun {retry 2}, logfile {path %q}, exit", logfile))
	r := o.results[0]
	assert.True(t, r.Failed)
	assert.Equal(t, 2, r.Reruns)
	assert.Equal(t, 3, count("rerun-never"), "first pass + 2 reruns")
	assert.Equal(t, 1, strings.Count(readFile(t, logfile), "\n"), "the actions run once, after the last rerun")
	assert.Equal(t, []string{"logfile", "exit"}, outcomeNames(r))
	assert.Equal(t, 1, o.code)
	contains(t, o.out, "RERUN  still failing after 2 rerun(s) (max 2)")
}

func TestRerunSettingsPrecedence(t *testing.T) {
	// compiled-in rerun_retry is 1
	o := run(t, "[a]\nck counter(\"rr-compiled\", n >= 100)\naction rerun")
	assert.Equal(t, 1, o.results[0].Reruns, "compiled-in")
	// [DEFAULTS] rerun_retry beats the compiled-in value; the section's own option beats both
	o = run(t, "[DEFAULTS] {rerun_retry = 3, rerun_sleep = 0.1}\n[a]\nck counter(\"rr-defaults\", n >= 100)\naction rerun\n[b]\nck counter(\"rr-own\", n >= 100)\naction rerun {retry 1}")
	assert.Equal(t, 3, o.results[0].Reruns)
	assert.Equal(t, 1, o.results[1].Reruns)
}

func TestRerunSleepsBeforeEachRerun(t *testing.T) {
	o := run(t, "[a]\nck counter(\"rr-sleep\", n >= 100)\naction rerun {retry 2, sleep 0.3}")
	assert.GreaterOrEqual(t, o.took, 550*time.Millisecond, "two sleeps of 0.3s")
}

func TestRerunTimeoutCutsOffARerunPass(t *testing.T) {
	o := run(t, "[a]\nck slowsecond(\"rr-timeout\", n == 1)\naction rerun {retry 1, timeout 1}")
	r := o.results[0]
	assert.True(t, r.Failed)
	assert.Equal(t, 1, r.Reruns)
	assert.GreaterOrEqual(t, o.took, 900*time.Millisecond)
	assert.LessOrEqual(t, o.took, 3*time.Second)
	contains(t, o.out, "TIMEOUT")
}

func TestRerunWithExitnowWaitsForTheReruns(t *testing.T) {
	key := "rr-exitnow"
	run(t, "[a]\nck counter(\""+key+"\", n >= 100)\nck counter(\""+key+"-2\", n >= 100)\naction rerun {retry 2}, exitnow")
	// Without rerun, exitnow would stop at the first failed ck and never call the second.
	assert.Equal(t, 3, count(key+"-2"), "exitnow waits for the reruns and runs every pass fully")
}

func TestRerunDoesNotRunWhenAnIfFails(t *testing.T) {
	key := "rr-if"
	o := run(t, "[a]\nif environ(\"VERIFIEN_SURELY_UNSET\", set == yes)\nck counter(\""+key+"\", n >= 100)\naction rerun {retry 3}, exit")
	assert.Equal(t, 0, o.results[0].Reruns)
	assert.Equal(t, 0, count(key))
	assert.Equal(t, 0, o.code)
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

func TestScriptReceivesTheReportOnStdin(t *testing.T) {
	out := filepath.Join(t.TempDir(), "stdin.json")
	o := run(t, fmt.Sprintf(`[ACTION_SCRIPTS]
as:rec = "cat > %s"
[web servers]
ck environ("VERIFIEN_SURELY_UNSET", {msg "first message"}, set == yes)
%s
ck environ("VERIFIEN_SURELY_UNSET_2", set == yes)
action as:rec
`, out, passing))
	require.Len(t, o.results[0].Actions, 1)
	assert.Empty(t, o.results[0].Actions[0].Err)
	var rep spec.Report
	require.NoError(t, json.Unmarshal([]byte(readFile(t, out)), &rep))
	assert.Equal(t, "verifien", rep.Program)
	assert.Equal(t, "web servers", rep.Section)
	assert.NotEmpty(t, rep.Time)
	require.Len(t, rep.Failures, 2, "exactly the 2 failing cks")
	assert.Equal(t, "first message", rep.Failures[0].Message)
	assert.True(t, strings.HasPrefix(rep.Failures[0].Ck, `environ("VERIFIEN_SURELY_UNSET"`), rep.Failures[0].Ck)
	assert.Contains(t, rep.Failures[1].Message, "is false")
}

func TestActionsRunAtTheSameTime(t *testing.T) {
	dir := t.TempDir()
	o := run(t, fmt.Sprintf(`[ACTION_SCRIPTS]
as:a = "sleep 1; echo a > %[1]s/a"
as:b = "sleep 1; echo b > %[1]s/b"
as:c = "sleep 1; echo c > %[1]s/c"
[x]
%[2]s
action as:a, as:b, as:c
`, dir, failing))
	assert.Less(t, o.took, 1800*time.Millisecond, "three 1s scripts must run in separate goroutines")
	for _, name := range []string{"a", "b", "c"} {
		readFile(t, filepath.Join(dir, name))
	}
}

func TestFailedActionIsReportedButDoesNotChangeTheStatus(t *testing.T) {
	o := run(t, `[ACTION_SCRIPTS]
as:bad = "echo boom >&2; exit 3"
[a]
`+failing+`
action as:bad`)
	assert.Equal(t, 0, o.code, "a failed action must not change the exit status")
	contains(t, o.out, "ACTION  as:bad  FAILED")
	contains(t, o.out, "boom")
}

func TestScriptTimeoutAndRetry(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs")
	o := run(t, fmt.Sprintf(`[ACTION_SCRIPTS]
as:slow = "echo x >> %s; sleep 30"
[a]
%s
action as:slow {timeout 1, retry 1, sleep 0.2}
`, marker, failing))
	assert.Equal(t, 2, strings.Count(readFile(t, marker), "x"), "script runs")
	assert.GreaterOrEqual(t, o.took, 2000*time.Millisecond)
	assert.LessOrEqual(t, o.took, 4*time.Second)
	contains(t, o.out, "timed out after 1s")
}

func TestLogfileAppendsOneJSONLinePerFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fail.log")
	cfg := fmt.Sprintf("[DEFAULTS] {logfile_path = %q}\n[a]\n%s\nck environ(\"VERIFIEN_SURELY_UNSET_2\", set == yes)\naction logfile\n", path, failing)
	run(t, cfg)
	run(t, cfg) // appends, never truncates
	lines := strings.Split(strings.TrimSpace(readFile(t, path)), "\n")
	require.Len(t, lines, 4)
	var l struct{ Time, Program, Section, Ck, Message string }
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &l))
	assert.Equal(t, "verifien", l.Program)
	assert.Equal(t, "a", l.Section)
	assert.NotEmpty(t, l.Ck)
	assert.NotEmpty(t, l.Message)
	assert.NotEmpty(t, l.Time)
}

// receiver records the requests it gets.
type receiver struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
	paths  []string
	auth   []string
}

func newReceiver(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *receiver {
	rc := &receiver{}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.bodies = append(rc.bodies, string(body))
		rc.paths = append(rc.paths, r.URL.Path)
		rc.auth = append(rc.auth, r.Header.Get("Authorization"))
		rc.mu.Unlock()
		if handler != nil {
			handler(w, r)
		}
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) requests() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.bodies)
}

func TestWebhookPostsTheReport(t *testing.T) {
	rc := newReceiver(t, nil)
	o := run(t, fmt.Sprintf("[a]\n%s\naction webhook {url %q}", failing, rc.URL+"/hook"))
	require.Equal(t, 1, rc.requests())
	assert.Equal(t, "/hook", rc.paths[0])
	var rep spec.Report
	require.NoError(t, json.Unmarshal([]byte(rc.bodies[0]), &rep))
	assert.Equal(t, "a", rep.Section)
	assert.Len(t, rep.Failures, 1)
	assert.Empty(t, o.results[0].Actions[0].Err)
}

func TestWebhookUsesDefaultsURL(t *testing.T) {
	rc := newReceiver(t, nil)
	run(t, fmt.Sprintf("[DEFAULTS] {webhook_url = %q}\n[a]\n%s\naction webhook", rc.URL+"/from-defaults", failing))
	require.Equal(t, 1, rc.requests())
	assert.Equal(t, "/from-defaults", rc.paths[0])
}

func TestWebhookRetriesServerErrors(t *testing.T) {
	rc := newReceiver(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	o := run(t, fmt.Sprintf("[DEFAULTS] {retry = 2, sleep = 0.1}\n[a]\n%s\naction webhook {url %q}", failing, rc.URL))
	assert.Equal(t, 3, rc.requests(), "1 + retry 2 from DEFAULTS")
	contains(t, o.out, "500")
}

func TestWebhookOptionBeatsDefaults(t *testing.T) {
	rc := newReceiver(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	run(t, fmt.Sprintf("[DEFAULTS] {retry = 4}\n[a]\n%s\naction webhook {url %q, retry 0}", failing, rc.URL))
	assert.Equal(t, 1, rc.requests())
}

func TestWebhookTimeoutAndRetry(t *testing.T) {
	var started atomic.Int32
	rc := newReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	})
	o := run(t, fmt.Sprintf("[a]\n%s\naction webhook {url %q, timeout 1, retry 1, sleep 0.3}", failing, rc.URL))
	assert.EqualValues(t, 2, started.Load())
	assert.GreaterOrEqual(t, o.took, 2200*time.Millisecond)
	assert.LessOrEqual(t, o.took, 4*time.Second)
	contains(t, o.out, "ACTION  webhook  FAILED: timed out after 1s")
}

func TestOpentelSendsOTLPLogs(t *testing.T) {
	rc := newReceiver(t, nil)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer%20tok123")
	run(t, fmt.Sprintf("[DEFAULTS] {otel_endpoint = %q, otel_service = \"svc-test\"}\n[web]\n%s\nck environ(\"VERIFIEN_SURELY_UNSET_2\", {msg \"second\"}, set == yes)\naction opentel", rc.URL, failing))
	require.Equal(t, 1, rc.requests())
	assert.Equal(t, "/v1/logs", rc.paths[0])
	assert.Equal(t, "Bearer tok123", rc.auth[0])
	var body struct {
		ResourceLogs []struct {
			Resource struct {
				Attributes []struct {
					Key   string
					Value struct{ StringValue string }
				}
			}
			ScopeLogs []struct {
				LogRecords []struct {
					TimeUnixNano   string
					SeverityNumber int
					SeverityText   string
					Body           struct{ StringValue string }
					Attributes     []struct {
						Key   string
						Value struct{ StringValue string }
					}
				}
			}
		}
	}
	require.NoError(t, json.Unmarshal([]byte(rc.bodies[0]), &body), rc.bodies[0])
	rl := body.ResourceLogs[0]
	assert.Equal(t, "service.name", rl.Resource.Attributes[0].Key)
	assert.Equal(t, "svc-test", rl.Resource.Attributes[0].Value.StringValue)
	recs := rl.ScopeLogs[0].LogRecords
	require.Len(t, recs, 2)
	assert.Equal(t, "ERROR", recs[0].SeverityText)
	assert.Equal(t, 17, recs[0].SeverityNumber)
	assert.Equal(t, "second", recs[1].Body.StringValue)
	assert.NotEmpty(t, recs[0].TimeUnixNano)
	assert.Equal(t, "verifien.section", recs[0].Attributes[0].Key)
	assert.Equal(t, "web", recs[0].Attributes[0].Value.StringValue)
}

func TestOpentelFallsBackToTheEnvironment(t *testing.T) {
	rc := newReceiver(t, nil)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", rc.URL+"/")
	run(t, "[a]\n"+failing+"\naction opentel")
	require.Equal(t, 1, rc.requests())
	assert.Equal(t, "/v1/logs", rc.paths[0])
}

func TestOpentelNeedsAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	_, _, err := load("[a]\n" + failing + "\naction opentel")
	assert.ErrorContains(t, err, "no endpoint")
}

func TestSyslogOverUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen on UDP:", err)
	}
	defer pc.Close()
	got := make(chan string, 4)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			got <- string(buf[:n])
		}
	}()
	run(t, fmt.Sprintf("[DEFAULTS] {syslog_address = \"udp://%s\", syslog_tag = \"vtest\", syslog_facility = local3}\n[web]\n%s\naction syslog", pc.LocalAddr(), failing))
	select {
	case msg := <-got:
		// <155> is local3 (19*8) + err (3)
		for _, want := range []string{"<155>", "vtest", `section="web"`, "VERIFIEN_SURELY_UNSET"} {
			assert.Contains(t, msg, want)
		}
	case <-time.After(3 * time.Second):
		require.Fail(t, "no syslog message arrived")
	}
}

// udpListener returns a UDP address and a channel of the datagrams it receives.
func udpListener(t *testing.T) (string, <-chan string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen on UDP:", err)
	}
	t.Cleanup(func() { pc.Close() })
	got := make(chan string, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			got <- string(buf[:n])
		}
	}()
	return pc.LocalAddr().String(), got
}

func nextDatagram(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(3 * time.Second):
		require.Fail(t, "no syslog message arrived")
		return ""
	}
}

func TestSyslogOptionsOverrideDefaultsPerUse(t *testing.T) {
	defAddr, defGot := udpListener(t)
	otherAddr, otherGot := udpListener(t)
	run(t, fmt.Sprintf(`[DEFAULTS] {syslog_address "udp://%s", syslog_tag "dflt", syslog_facility local3}
[plain]
%s
action syslog
[tagged]
%s
action syslog {syslog_tag "mine", syslog_facility daemon}
[elsewhere]
%s
action syslog {syslog_address "udp://%s", syslog_facility local0}
`, defAddr, failing, failing, failing, otherAddr))

	// from [plain]: everything from [DEFAULTS]. [tagged] changes tag and facility
	// only, so it also arrives at the default address.
	var msgs []string
	for i := 0; i < 2; i++ {
		msgs = append(msgs, nextDatagram(t, defGot))
	}
	var plain, tagged string
	for _, m := range msgs {
		if strings.Contains(m, `section="plain"`) {
			plain = m
		}
		if strings.Contains(m, `section="tagged"`) {
			tagged = m
		}
	}
	require.NotEmpty(t, plain, msgs)
	require.NotEmpty(t, tagged, msgs)
	assert.Contains(t, plain, "<155>") // local3 (19*8) + err (3)
	assert.Contains(t, plain, " dflt[")
	assert.Contains(t, tagged, "<27>") // daemon (3*8) + err (3)
	assert.Contains(t, tagged, " mine[")

	// [elsewhere]: its own address, facility local0, and the tag from [DEFAULTS]
	m := nextDatagram(t, otherGot)
	assert.Contains(t, m, `section="elsewhere"`)
	assert.Contains(t, m, "<131>") // local0 (16*8) + err (3)
	assert.Contains(t, m, " dflt[")
}

func TestSyslogOptionsAreValidated(t *testing.T) {
	_, _, err := load("[a]\n" + failing + "\naction syslog {syslog_facility nope}")
	assert.ErrorContains(t, err, "must be one of")
	_, _, err = load("[a]\n" + failing + "\naction syslog {syslog_address \"host:514\"}")
	assert.ErrorContains(t, err, "udp://host:514")
	_, _, err = load("[a]\n" + failing + "\naction syslog {bogus 1}")
	assert.ErrorContains(t, err, `no option "bogus"`)
	mustLoad(t, "[a]\n"+failing+"\naction syslog {syslog_tag \"x\", syslog_facility local7, syslog_address \"tcp://h:514\", timeout 2, retry 1}")
}

// ---------------------------------------------------------------------------
// {options} on a check of a variable: ck var.item OP value {msg "..."}
// ---------------------------------------------------------------------------

func TestMsgOnACheckOfAVariable(t *testing.T) {
	cfg := "[a]\nvar = stat(\"/\", {retry=0})\n\tck var.type == f {msg = \"/ is not a file\"}\n" +
		"ck var.type == d {msg \"never shown\"}\n" +
		"ck var.size >= 0\n"
	o := run(t, cfg)
	r := o.results[0]
	require.Len(t, r.Stmts, 4)
	assert.False(t, r.Stmts[1].Pass)
	assert.Equal(t, "/ is not a file", r.Stmts[1].Detail, "msg replaces the built-in message")
	assert.True(t, r.Stmts[2].Pass, "msg changes nothing when the check passes")
	assert.True(t, r.Stmts[3].Pass, "a check without options still works")
	contains(t, o.out, "-> / is not a file")
	assert.True(t, r.Failed)
}

func TestMsgOnAnIfOfAVariable(t *testing.T) {
	// an if with options parses, gates the cks as usual, and does not break the
	// statements after it
	cfg := "[a]\nvar = stat(\"/\")\nif var.type == f {msg \"not a file\"}\n" + failing + "\naction exit"
	o := run(t, cfg)
	assert.True(t, o.results[0].Skipped)
	assert.Equal(t, 0, o.code)
	o = run(t, "[a]\nvar = stat(\"/\")\nif var.type == d {msg \"unused\"}\n"+failing+"\naction exit")
	assert.False(t, o.results[0].Skipped)
	assert.Equal(t, 1, o.code)
}

func TestMsgOnACheckAlsoReplacesAFailedAssignment(t *testing.T) {
	o := run(t, "[a]\nv = stat(\"/nonexistent/x\")\nck v.type == f {msg \"cannot look at it\"}")
	assert.Equal(t, "cannot look at it", o.results[0].Stmts[1].Detail)
}

// Only msg means anything on a check of a variable; any other option is ignored,
// not an error (the same way environ ignores timeout, retry and sleep).
func TestOtherOptionsOnACheckAreIgnored(t *testing.T) {
	pre := "[a]\nv = stat(\"/\")\n"
	mustLoad(t, pre+"ck v.type == d {timeout 3}")
	mustLoad(t, pre+"if v.type == d {retry 1, sleep 0.5, bogus \"x\"}")
	mustLoad(t, pre+"ck v.type == d {}\nck v.size > 0 {msg=\"x\"}\naction exit")

	// the ignored options change nothing, and msg next to them still works
	o := run(t, pre+"ck v.type == f {timeout 1, retry 5, whatever yes, msg \"not a file\"}")
	assert.Equal(t, "not a file", o.results[0].Stmts[1].Detail)
	o = run(t, pre+"ck v.type == f {retry 5}")
	assert.Contains(t, o.results[0].Stmts[1].Detail, "is false", "without msg the built-in message stays")
}

func TestMsgOnACheckMustStillBeAString(t *testing.T) {
	_, _, err := load("[a]\nv = stat(\"/\")\nck v.type == d {msg 3}")
	assert.ErrorContains(t, err, "msg")
}
