package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func allPass(t *testing.T, cfg string) outcome {
	t.Helper()
	o := run(t, cfg)
	for i, st := range o.results[0].Stmts {
		assert.True(t, st.Pass, "statement %d %s: %s", i, st.Text, st.Detail)
	}
	return o
}

// The request: /etc is a link to private/etc, which is relative to /, so the
// real place is /private/etc. Here the "/" is a temp directory.
func TestSedMakesALinkTargetAbsolute(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "private", "etc"), 0o755))
	require.NoError(t, os.Chmod(filepath.Join(root, "private", "etc"), 0o755))
	require.NoError(t, os.Symlink("private/etc", filepath.Join(root, "etc")))
	o := allPass(t, fmt.Sprintf(`[a]
var = lstat(%[1]q)
ck var.type == l
ck var.link == "private/etc"
var2 = sed(var.link, "s/^/\//", "s#^#%[2]s#")
ck var2.value == "%[2]s/private/etc"
ck lstat(var2.value, type == d, perm == 0755)
`, filepath.Join(root, "etc"), root))
	assert.Contains(t, o.out, `sed(var.link, "s/^/\\//"`)
	// \/ in a quoted string reaches sed as \/ (no doubling needed)
	f, _ := mustLoad(t, `[a]`+"\n"+`v = sed("x", "s/^/\//")`)
	assert.Equal(t, []string{`s/^/\//`}, f.Sections()[0].Statements[0].Assign.Call.Args)
}

func TestSedOnNumbersAndPermissions(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	require.NoError(t, os.WriteFile(a, []byte("12345"), 0o640))
	require.NoError(t, os.Chmod(a, 0o640))
	require.NoError(t, os.WriteFile(b, []byte("12345"), 0o640))
	require.NoError(t, os.Chmod(b, 0o640))
	// a permission is its octal text; sed can reshape it
	allPass(t, fmt.Sprintf(`[a]
x = lstat(%q)
p = sed(x.perm, "s/^0//")
ck p.value == "640"
q = sed(x.perm, "s/^/-/")
ck q.value == "-0640"
ck lstat(%q, perm == q.value)
ck lstat(%q, perm == x.perm)
n = sed(x.size, "s/5/6/")
ck n.value == "6"
`, a, b, b))
	// text that parses as a number is a number for a typed item; the check is made when it runs
	allPass(t, fmt.Sprintf(`[a]
x = lstat(%q)
sz = sed(x.size, "s/$/0/")
ck lstat(%q, size < sz.value)
u = sed(x.uid, "s/^/0/")
ck lstat(%q, uid == u.value)
`, a, b, b))
	// text that is not usable for the item fails the check, with a reason, not the program
	o := run(t, fmt.Sprintf(`[a]
x = lstat(%q)
w = sed(x.name, "s/^/zz/")
ck lstat(%q, perm == w.value)
ck lstat(%q, size == w.value)
ck lstat(%q, type == w.value)
`, a, b, b, b))
	for _, st := range o.results[0].Stmts[2:] {
		assert.False(t, st.Pass, st.Text)
		assert.Contains(t, st.Detail, "is not usable here", st.Text)
	}
	// any item may be the text for sed; it is a number for sed only; others need text
	mustLoad(t, `[a]`+"\n"+`x = lstat("/")`+"\n"+`ck sed(x.size, "s/1/2/", value == "2")`)
	_, _, err := load(`[a]` + "\n" + `x = lstat("/")` + "\n" + `ck environ(x.size)`)
	assert.ErrorContains(t, err, "is not text")
}

func TestSedInAChain(t *testing.T) {
	t.Setenv("VERIFIEN_SED_PATH", "/Users/me/Library")
	allPass(t, `[a]
e = environ("VERIFIEN_SED_PATH")
s1 = sed(e.value, "s,^/Users/[^/]+,/home/me,", "s/Library/lib/")
ck s1.value == "/home/me/lib"
ck sed("a-b-c", "y/-/_/", "s/_/-/2", value == "a_b-c")
ck ! sed("abc", "s/b/X/", value == "abc")
`)
}

func TestSedConfigErrors(t *testing.T) {
	for name, c := range map[string]struct{ cfg, want string }{
		"no commands":        {"[a]\nv = sed(\"x\")", "needs at least one command"},
		"bad command":        {"[a]\nv = sed(\"x\", \"d\")", "only s and y"},
		"bad regex":          {"[a]\nv = sed(\"x\", \"s/(/y/\")", "bad regular expression"},
		"extra string given": {"[a]\nv = lstat(\"/\", \"s/a/b/\")", "takes only one string"},
		"unknown item":       {"[a]\nv = sed(\"x\", \"s/a/b/\")\nck v.result == \"x\"", "has no test"},
		"error has the line": {"[a]\n\nv = sed(\"x\", \"s/a\")", "test.cfg:3:"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := load(c.cfg)
			require.Error(t, err)
			assert.ErrorContains(t, err, c.want)
		})
	}
	// options and tests still follow the strings
	mustLoad(t, "[a]\nck sed(\"x\", \"s/x/y/\", \"s/y/z/\", {msg \"m\"}, value == \"z\")")
}
