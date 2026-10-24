package main

// Tests for include / sinclude, the braces around [DEFAULTS], and the stdout
// action.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"verifien/action_stdout"
	"verifien/spec"
)

// write puts the files (name -> text) in a new directory and returns it.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		p := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(text), 0o644))
	}
	return dir
}

// loadFrom parses the config file at path, as main does.
func loadFrom(t *testing.T, path string) (*File, Settings, error) {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return parseAndValidate(buildParser(), path, string(b))
}

func sectionNames(f *File) []string {
	var n []string
	for _, s := range f.Sections() {
		n = append(n, s.Title())
	}
	return n
}

func TestDefaultsMustBeInBraces(t *testing.T) {
	_, _, err := load("[DEFAULTS]\nretry = 2\n[a]\n" + failing)
	assert.Error(t, err, "bare key = value lines are no longer accepted")
	mustLoad(t, "[DEFAULTS] {retry = 2}\n[a]\n"+failing)
	mustLoad(t, "[DEFAULTS] {}\n[a]\n"+failing)
	mustLoad(t, "[DEFAULTS] {\n  retry 2,   # no equals sign needed\n  sleep = 0.5\n}\n[a]\n"+failing)
	// commas are required, as in the options of a call
	_, _, err = load("[DEFAULTS] {retry 2 sleep 1}\n[a]\n" + failing)
	assert.Error(t, err)
}

func TestIncludeSplicesSectionsInPlace(t *testing.T) {
	dir := write(t, map[string]string{
		"verifien.cfg": "[first]\n" + passing + "\ninclude \"web.cfg\"\n[last]\n" + passing + "\n",
		"web.cfg":      "[web1]\n" + passing + "\n[web2]\n" + passing + "\n",
	})
	f, _, err := loadFrom(t, filepath.Join(dir, "verifien.cfg"))
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "web1", "web2", "last"}, sectionNames(f))
}

func TestIncludeFailsWhenTheFileIsMissingButSincludeDoesNot(t *testing.T) {
	dir := write(t, map[string]string{
		"a.cfg": "include \"nope.cfg\"\n[a]\n" + passing + "\n",
		"b.cfg": "sinclude \"nope.cfg\"\n[b]\n" + passing + "\n",
	})
	_, _, err := loadFrom(t, filepath.Join(dir, "a.cfg"))
	assert.ErrorContains(t, err, "nope.cfg")
	assert.ErrorContains(t, err, "a.cfg:1:")
	f, _, err := loadFrom(t, filepath.Join(dir, "b.cfg"))
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, sectionNames(f))
}

func TestSincludeStillReportsErrorsInAFileThatExists(t *testing.T) {
	dir := write(t, map[string]string{
		"main.cfg": "sinclude \"bad.cfg\"\n[a]\n" + passing + "\n",
		"bad.cfg":  "[b]\nck nosuchfn(\"x\", y == 1)\n",
	})
	_, _, err := loadFrom(t, filepath.Join(dir, "main.cfg"))
	assert.ErrorContains(t, err, "nosuchfn")
}

func TestIncludePathsAreRelativeToTheIncludingFile(t *testing.T) {
	dir := write(t, map[string]string{
		"main.cfg":    "include \"sub/one.cfg\"\n",
		"sub/one.cfg": "include \"two.cfg\"\n[one]\n" + passing + "\n",
		"sub/two.cfg": "[two]\n" + passing + "\n",
		"two.cfg":     "[WRONG]\n" + passing + "\n",
	})
	f, _, err := loadFrom(t, filepath.Join(dir, "main.cfg"))
	require.NoError(t, err)
	assert.Equal(t, []string{"two", "one"}, sectionNames(f))
}

func TestIncludeLoopIsAnError(t *testing.T) {
	dir := write(t, map[string]string{
		"a.cfg": "include \"b.cfg\"\n",
		"b.cfg": "include \"a.cfg\"\n[b]\n" + passing + "\n",
	})
	_, _, err := loadFrom(t, filepath.Join(dir, "a.cfg"))
	assert.ErrorContains(t, err, "loop")
}

func TestIncludeMergesDefaultsAndTheIncludingFileWins(t *testing.T) {
	dir := write(t, map[string]string{
		"main.cfg": "[DEFAULTS] {retry = 5, exit_status_bad = 9}\ninclude \"inc.cfg\"\n[a]\n" + passing + "\n",
		"inc.cfg":  "[DEFAULTS] {retry = 1, sleep = 0.5, exit_status_now = 7}\n[b]\n" + passing + "\n",
	})
	_, set, err := loadFrom(t, filepath.Join(dir, "main.cfg"))
	require.NoError(t, err)
	assert.Equal(t, 5, set.Retry, "the including file's value wins")
	assert.Equal(t, 9, set.ExitBad)
	assert.Equal(t, 7, set.ExitNow, "a key only the included file sets is kept")
	assert.InDelta(t, 0.5, set.Sleep.Seconds(), 0.001)
}

func TestIncludeLaterIncludeBeatsEarlier(t *testing.T) {
	dir := write(t, map[string]string{
		"main.cfg": "include \"one.cfg\"\ninclude \"two.cfg\"\n",
		"one.cfg":  "[DEFAULTS] {retry = 1}\n",
		"two.cfg":  "[DEFAULTS] {retry = 2}\n",
	})
	_, set, err := loadFrom(t, filepath.Join(dir, "main.cfg"))
	require.NoError(t, err)
	assert.Equal(t, 2, set.Retry)
}

func TestIncludeMergesScriptsAndRejectsDuplicates(t *testing.T) {
	dir := write(t, map[string]string{
		"ok.cfg":  "[ACTION_SCRIPTS]\nas:one = \"true\"\ninclude \"inc.cfg\"\n",
		"inc.cfg": "[ACTION_SCRIPTS]\nas:two = \"true\"\n",
		"dup.cfg": "[ACTION_SCRIPTS]\nas:two = \"false\"\ninclude \"inc.cfg\"\n",
	})
	_, set, err := loadFrom(t, filepath.Join(dir, "ok.cfg"))
	require.NoError(t, err)
	assert.Len(t, set.Scripts, 2)
	_, _, err = loadFrom(t, filepath.Join(dir, "dup.cfg"))
	assert.ErrorContains(t, err, "defined twice")
}

func TestIncludeErrorsNameTheIncludedFile(t *testing.T) {
	dir := write(t, map[string]string{
		"main.cfg": "include \"inc.cfg\"\n",
		"inc.cfg":  "[a]\n" + passing + "\nck bogus\n",
	})
	_, _, err := loadFrom(t, filepath.Join(dir, "main.cfg"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inc.cfg:")
}

func TestIncludeAUrl(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case "/team.cfg":
			w.Write([]byte("[remote]\n" + passing + "\ninclude \"more.cfg\"\n"))
		case "/more.cfg": // relative to the URL it came from
			w.Write([]byte("[more]\n" + passing + "\n"))
		case "/boom.cfg":
			w.WriteHeader(500)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	f, _, err := load("include \"" + srv.URL + "/team.cfg\"\n[local]\n" + passing + "\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"remote", "more", "local"}, sectionNames(f))

	_, _, err = load("include \"" + srv.URL + "/missing.cfg\"\n")
	assert.ErrorContains(t, err, "404")
	f, _, err = load("sinclude \"" + srv.URL + "/missing.cfg\"\n[local]\n" + passing + "\n")
	require.NoError(t, err, "sinclude skips a 404")
	assert.Equal(t, []string{"local"}, sectionNames(f))

	_, _, err = load("sinclude \"" + srv.URL + "/boom.cfg\"\n")
	assert.ErrorContains(t, err, "500", "sinclude skips only what is not there, not a server error")
}

func TestIncludeFromAUrlCannotReadLocalFiles(t *testing.T) {
	local := write(t, map[string]string{"secret.cfg": "[secret]\n" + passing + "\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("include \"" + filepath.Join(local, "secret.cfg") + "\"\n"))
	}))
	defer srv.Close()
	// the path is resolved as a URL path on the server, never as a local file
	// (here that makes the server's reply include itself, which is a loop)
	_, _, err := load("include \"" + srv.URL + "/x.cfg\"\n")
	assert.ErrorContains(t, err, "loop")
}

func TestIncludeRejectsOtherSchemes(t *testing.T) {
	_, _, err := load("include \"ftp://example.com/x.cfg\"\n")
	assert.ErrorContains(t, err, "http://")
}

func TestStdoutActionWritesOneJSONLinePerSection(t *testing.T) {
	var buf bytes.Buffer
	old := actionstdout.Out
	actionstdout.Out = &buf
	defer func() { actionstdout.Out = old }()

	o := run(t, "[web]\n"+failing+"\nck environ(\"VERIFIEN_SURELY_UNSET_2\", {msg \"second\"}, set == yes)\naction stdout\n[db]\n"+failing+"\naction stdout")
	assert.Equal(t, 0, o.code)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 2, buf.String())
	got := map[string]spec.Report{}
	for _, l := range lines {
		var rep spec.Report
		require.NoError(t, json.Unmarshal([]byte(l), &rep), l)
		got[rep.Section] = rep
	}
	assert.Equal(t, "verifien", got["web"].Program)
	assert.Len(t, got["web"].Failures, 2)
	assert.Equal(t, "second", got["web"].Failures[1].Message)
	assert.Len(t, got["db"].Failures, 1)
}

// ---------------------------------------------------------------------------
// Command line: --cfg and --debug
// ---------------------------------------------------------------------------

func TestParseArgs(t *testing.T) {
	var sink bytes.Buffer
	o, err := parseArgs(nil, &sink)
	require.NoError(t, err)
	assert.Equal(t, options{}, o)

	o, err = parseArgs([]string{"--debug", "--cfg", "x.cfg"}, &sink)
	require.NoError(t, err)
	assert.Equal(t, options{cfg: "x.cfg", debug: true}, o)

	o, err = parseArgs([]string{"-cfg=https://example.com/a.cfg"}, &sink)
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/a.cfg", o.cfg)

	_, err = parseArgs([]string{"--bogus"}, &sink)
	assert.Error(t, err)
	_, err = parseArgs([]string{"stray"}, &sink)
	assert.ErrorContains(t, err, "unexpected argument")
}

func TestCfgFileReplacesTheDefaultCompletely(t *testing.T) {
	dir := write(t, map[string]string{
		"verifien.cfg": "[from default]\n" + passing + "\n",
		"other.cfg":    "[from cfg]\n" + passing + "\n",
	})
	t.Chdir(dir)

	f, _, err := loadConfig(buildParser(), "")
	require.NoError(t, err)
	assert.Equal(t, []string{"from default"}, sectionNames(f), "no --cfg: ./verifien.cfg")

	f, _, err = loadConfig(buildParser(), "other.cfg")
	require.NoError(t, err)
	assert.Equal(t, []string{"from cfg"}, sectionNames(f), "--cfg: verifien.cfg is not read at all")

	// not even when verifien.cfg is broken
	require.NoError(t, os.WriteFile("verifien.cfg", []byte("this is not a config"), 0o644))
	f, _, err = loadConfig(buildParser(), "other.cfg")
	require.NoError(t, err)
	assert.Equal(t, []string{"from cfg"}, sectionNames(f))
}

func TestCfgMissingFileIsAnError(t *testing.T) {
	t.Chdir(t.TempDir())
	_, _, err := loadConfig(buildParser(), "")
	assert.Error(t, err)
	_, _, err = loadConfig(buildParser(), "nope.cfg")
	assert.ErrorContains(t, err, "nope.cfg")
}

func TestCfgUrlAndItsRelativeIncludes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conf/main.cfg":
			w.Write([]byte("include \"part.cfg\"\n[main]\n" + passing + "\n"))
		case "/conf/part.cfg":
			w.Write([]byte("[part]\n" + passing + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Chdir(write(t, map[string]string{"verifien.cfg": "[ignored]\n" + passing + "\n"}))

	f, _, err := loadConfig(buildParser(), srv.URL+"/conf/main.cfg")
	require.NoError(t, err)
	assert.Equal(t, []string{"part", "main"}, sectionNames(f))

	_, _, err = loadConfig(buildParser(), srv.URL+"/conf/missing.cfg")
	assert.ErrorContains(t, err, "404")
}

func TestManyIncludesAreReadInOrder(t *testing.T) {
	dir := write(t, map[string]string{
		"main.cfg": "[first]\n" + passing + "\ninclude \"a.cfg\"\nsinclude \"gone.cfg\"\n[middle]\n" + passing + "\ninclude \"b.cfg\"\ninclude \"c.cfg\"\n[last]\n" + passing + "\n",
		"a.cfg":    "[a]\n" + passing + "\n",
		"b.cfg":    "[b]\n" + passing + "\n",
		"c.cfg":    "[c]\n" + passing + "\n",
	})
	f, _, err := loadFrom(t, filepath.Join(dir, "main.cfg"))
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "a", "middle", "b", "c", "last"}, sectionNames(f))
}

func TestReportIsOnlyPrintedWithDebug(t *testing.T) {
	f, set := mustLoad(t, "[a]\n"+failing+"\naction exit")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	results := runAll(ctx, f, set, stop)

	// report writes to os.Stdout; capture it
	old := os.Stdout
	rd, wr, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = wr
	quiet := report(results, set, false)
	loud := report(results, set, true)
	wr.Close()
	os.Stdout = old
	got, _ := io.ReadAll(rd)

	assert.Equal(t, 1, quiet, "the exit status does not depend on --debug")
	assert.Equal(t, 1, loud)
	assert.Equal(t, 1, strings.Count(string(got), "FAIL"), "printed once, by the debug call only:\n%s", got)
}
