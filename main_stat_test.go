package main

// End-to-end tests for the ! (negation) of if and ck, which works for every
// function, and for the stat items written in a config file.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stmtPass(t *testing.T, cfg string) (bool, string) {
	t.Helper()
	o := run(t, cfg)
	r := o.results[0]
	if r.Skipped {
		return false, "skipped"
	}
	last := r.Stmts[len(r.Stmts)-1]
	return last.Pass, last.Detail
}

func TestNotWorksForEveryFunction(t *testing.T) {
	for name, c := range map[string]struct {
		stmt string
		pass bool
	}{
		// stat: the file does not exist, so the call errors, so ! passes
		"stat missing":       {`ck ! lstat("/nonexistent/x", type == f)`, true},
		"stat missing no ts": {`ck ! lstat("/nonexistent/x")`, true},
		"stat exists":        {`ck ! lstat("/", type == d)`, false},
		"stat other type":    {`ck ! lstat("/", type == f)`, true},
		// environ
		"environ unset":     {`ck ! environ("VERIFIEN_SURELY_UNSET", set == yes)`, true},
		"environ set":       {`ck ! environ("PATH", set == yes)`, false},
		"environ two tests": {`ck ! environ("PATH", set == yes, value == "no-such-path")`, true},
		"no space after !":  {`ck !environ("PATH", set == yes)`, false},
		// a function that is not in the repo's real set: the test-only counter
		"counter":        {`ck ! counter("not-1", n >= 5)`, true},
		"counter true":   {`ck ! counter("not-2", n >= 1)`, false},
		"if is the same": {`if ! environ("PATH", set == yes)` + "\n" + `ck environ("PATH", set == yes)`, false},
	} {
		t.Run(name, func(t *testing.T) {
			pass, _ := stmtPass(t, "[a]\n"+c.stmt)
			if name == "if is the same" {
				// the if fails, so the section is skipped
				o := run(t, "[a]\n"+c.stmt)
				assert.True(t, o.results[0].Skipped)
				return
			}
			assert.Equal(t, c.pass, pass)
		})
	}
}

func TestNotInTheReportAndWithMsg(t *testing.T) {
	o := run(t, "[a]\nck ! environ(\"PATH\", set == yes)\nck ! environ(\"PATH\", {msg \"PATH must not be set\"}, set == yes)")
	contains(t, o.out, `! environ("PATH", set == yes)  {timeout 10, retry 0, sleep 0}`)
	contains(t, o.out, "the statement is true, but the ! says it must not be")
	contains(t, o.out, "-> PATH must not be set")
}

func TestIfNotGatesTheChecks(t *testing.T) {
	// the file is absent, so "if ! stat" holds and the check runs (and fails)
	o := run(t, "[a]\nif ! lstat(\"/shouldnotexist\", type == f)\n"+failing+"\naction exit")
	assert.False(t, o.results[0].Skipped)
	assert.Equal(t, 1, o.code)
	// the file is there, so "if ! stat" fails and the section is skipped
	o = run(t, "[a]\nif ! lstat(\"/\", type == d)\n"+failing+"\naction exit")
	assert.True(t, o.results[0].Skipped)
	assert.Equal(t, 0, o.code)
}

func TestNotOnACheckOfAVariable(t *testing.T) {
	for stmt, pass := range map[string]bool{
		`ck ! s.type == f`:                   true,
		`ck ! s.type == d`:                   false,
		`ck ! s.size > 1T`:                   true,
		`ck ! s.type == d {msg "not a dir"}`: false,
	} {
		got, _ := stmtPass(t, "[a]\ns = lstat(\"/\")\n"+stmt)
		assert.Equal(t, pass, got, stmt)
	}
	// a failed assignment: the variable has no result, which is "not true"
	got, _ := stmtPass(t, "[a]\ns = lstat(\"/nonexistent/x\")\nck ! s.type == f")
	assert.True(t, got)
	o := run(t, "[a]\ns = lstat(\"/\")\nck ! s.type == d {msg \"must not be a dir\"}")
	contains(t, o.out, "-> must not be a dir")
}

func TestNotWithAnActionAndRerun(t *testing.T) {
	// exit fires only because the negated check failed
	o := run(t, "[a]\nck ! environ(\"PATH\", set == yes)\naction exit")
	assert.Equal(t, 1, o.code)
	o = run(t, "[a]\nck ! environ(\"VERIFIEN_SURELY_UNSET\", set == yes)\naction exit")
	assert.Equal(t, 0, o.code)
}

// ---------------------------------------------------------------------------
// The new stat items, written the way a config file writes them
// ---------------------------------------------------------------------------

func TestStatItemsInAConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, []byte("hello"), 0o640))
	require.NoError(t, os.Chmod(file, 0o640))
	require.NoError(t, os.Symlink("f", filepath.Join(dir, "ln")))
	require.NoError(t, os.Link(file, filepath.Join(dir, "hard")))
	fifo := filepath.Join(dir, "fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	big := filepath.Join(dir, "big")
	bf, err := os.Create(big)
	require.NoError(t, err)
	bigOK := bf.Truncate(5<<40) == nil
	bf.Close()

	cks := []string{
		fmt.Sprintf(`ck lstat(%q, type == f, size == 5, nlink == 2, link == "", perm == 0640)`, file),
		fmt.Sprintf(`ck lstat(%q, perm == "u=rw,g=r,o=")`, file),
		fmt.Sprintf(`ck lstat(%q, perm == "-0440", perm == "+0040", perm != "+0007")`, file),
		fmt.Sprintf(`ck lstat(%q, perm == "-u+r")`, file),
		fmt.Sprintf(`ck lstat(%q, uid == %d, gid == %d)`, file, os.Getuid(), os.Getgid()),
		fmt.Sprintf(`ck lstat(%q, type == l, link == "f", link =~ /^f$/)`, filepath.Join(dir, "ln")),
		fmt.Sprintf(`ck lstat(%q, type == p)`, fifo),
		fmt.Sprintf(`ck lstat("/dev/null", type == c)`),
		fmt.Sprintf(`ck ! lstat(%q, type == s)`, file),
		fmt.Sprintf(`ck ! lstat(%q, perm == "+0022")`, file),
	}
	if bigOK {
		cks = append(cks, fmt.Sprintf(`ck lstat(%q, size > 4T, size < 6T, size >= 5T, size != 4T)`, big))
	}
	for _, ck := range cks {
		pass, why := stmtPass(t, "[a]\n"+ck)
		assert.True(t, pass, "%s: %s", ck, why)
	}

	// and failures say what was found
	o := run(t, fmt.Sprintf("[a]\nck lstat(%q, perm == 0600)\nck lstat(%q, nlink == 5)", file, file))
	contains(t, o.out, "perm == 0600 is false (got 0640)")
	contains(t, o.out, "nlink == 5 is false (got 2)")
}

func TestStatItemErrorsInAConfig(t *testing.T) {
	for stmt, want := range map[string]string{
		`ck lstat("/", perm == 0888)`:        "octal",
		`ck lstat("/", perm > 0644)`:         "not allowed",
		`ck lstat("/", perm == "u+z")`:       "invalid symbolic mode",
		`ck lstat("/", uid > 0)`:             "not allowed",
		`ck lstat("/", gid =~ /x/)`:          "not allowed",
		`ck lstat("/", size > 99999999999T)`: "invalid size",
		`ck lstat("/", type == x)`:           "must be one of",
		`ck lstat("/", nlink == "x")`:        "number",
	} {
		_, _, err := load("[a]\n" + stmt)
		assert.ErrorContains(t, err, want, stmt)
	}
	// accepted: names that may exist only where it runs
	mustLoad(t, "[a]\nck lstat(\"/\", uid == nosuchuser, gid == \"nosuchgroup\")")
	_, _, err := load("[a]\nck lstat(\"/\", uid == " + "1)")
	assert.NoError(t, err)
	o := run(t, "[a]\nck lstat(\"/\", uid == nosuchuser)")
	contains(t, o.out, `no user "nosuchuser"`)
}

func TestACallWithNoTargetAndNegation(t *testing.T) {
	// fn() has an empty target; ! works with any function, including ones that
	// do not exist yet (see disk, which is a placeholder that reports an error)
	_, _, err := load("[a]\nif ! disk()\nck ! disk()\nck environ()\n")
	assert.NoError(t, err)
	o := run(t, "[a]\nck ! environ()")
	assert.True(t, o.results[0].Stmts[0].Pass, "environ() has no name, so it errors, so ! passes")
	o = run(t, "[a]\nck environ()")
	assert.False(t, o.results[0].Stmts[0].Pass)
	contains(t, o.out, "environ()")
	// an unknown function is still a config error, negated or not
	_, _, err = load("[a]\nck ! nosuchfn()")
	assert.ErrorContains(t, err, `unknown function "nosuchfn"`)
}

// ---------------------------------------------------------------------------
// An item of a variable as the target of a later call: lstat(var.link, ...)
// ---------------------------------------------------------------------------

func TestTargetFromAVariableItem(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "private.etc"), 0o755))
	require.NoError(t, os.Chmod(filepath.Join(dir, "private.etc"), 0o755))
	etc := filepath.Join(dir, "etc")
	require.NoError(t, os.Symlink(filepath.Join(dir, "private.etc"), etc))
	t.Chdir(dir)
	uid, gid := os.Getuid(), os.Getgid()

	// the shape of the request: look at a link, then at what it points to
	cfg := fmt.Sprintf(`[a]
var = lstat(%q)
ck var.type == l
ck var.link == %q
ck lstat(var.link, type == d, uid == %d, gid == %d, perm == 0755)
`, etc, filepath.Join(dir, "private.etc"), uid, gid)
	o := run(t, cfg)
	for i, st := range o.results[0].Stmts {
		assert.True(t, st.Pass, "statement %d %s: %s", i, st.Text, st.Detail)
	}
	contains(t, o.out, "lstat(var.link, type == d")

	// a relative link target resolves from the working directory, as the
	// function sees the string; here it is the name "private.etc" inside dir
	require.NoError(t, os.Remove(etc))
	require.NoError(t, os.Symlink("private.etc", etc))
	o = run(t, fmt.Sprintf("[a]\nvar = lstat(%q)\nck lstat(var.link, type == d)", etc))
	assert.True(t, o.results[0].Stmts[1].Pass, o.out)
}

func TestTargetFromAnyFunctionsItems(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VERIFIEN_TEST_DIR", dir)
	// environ -> stat: the value of a variable names the thing to look at
	o := run(t, "[a]\ne = environ(\"VERIFIEN_TEST_DIR\")\nck lstat(e.value, type == d)")
	assert.True(t, o.results[0].Stmts[1].Pass, o.out)
	// only text items can be a target: a number or permission is a config error
	for _, item := range []string{"c.n", "s.size", "s.perm", "s.uid", "s.type"} {
		_, _, err := load("[a]\nc = counter(\"x\")\ns = lstat(\"/\")\nck lstat(" + item + ", type == f)")
		assert.ErrorContains(t, err, "is not text", item)
	}
}

func TestCompareItemsOfTwoInodes(t *testing.T) {
	t.Setenv("VERIFIEN_CMP", "same")
	dir := t.TempDir()
	a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	for p, mode := range map[string]os.FileMode{a: 0o640, b: 0o640, c: 0o600} {
		require.NoError(t, os.WriteFile(p, []byte("12345"), mode))
		require.NoError(t, os.Chmod(p, mode))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big"), []byte("123456789"), 0o640))
	pre := fmt.Sprintf("[s]\nx = lstat(%q)\n", a)
	pass := func(line string, want bool) {
		t.Helper()
		o := run(t, pre+line)
		last := o.results[0].Stmts[len(o.results[0].Stmts)-1]
		assert.Equal(t, want, last.Pass, "%s: %s", line, last.Detail)
	}
	pass(fmt.Sprintf("ck lstat(%q, perm == x.perm)", b), true)
	pass(fmt.Sprintf("ck lstat(%q, perm == x.perm)", c), false)
	pass(fmt.Sprintf("ck lstat(%q, perm != x.perm, uid == x.uid, gid == x.gid)", c), true)
	pass(fmt.Sprintf("ck lstat(%q, size == x.size)", b), true)
	pass(fmt.Sprintf("ck lstat(%q, size > x.size)", filepath.Join(dir, "big")), true)
	pass(fmt.Sprintf("ck lstat(%q, name == x.name)", b), false)
	pass(fmt.Sprintf("if lstat(%q, type == x.type)", b), true)
	pass(fmt.Sprintf("ck ! lstat(%q, perm == x.perm)", c), true)
	// the check form: item OP variable.item
	pass(fmt.Sprintf("y = lstat(%q)\nck y.perm == x.perm", b), true)
	pass(fmt.Sprintf("y = lstat(%q)\nck y.perm == x.perm", c), false)
	// a failed variable makes the comparison fail, with a reason
	o := run(t, "[s]\nx = lstat(\"/nonexistent/q\")\nck lstat(\"/\", perm == x.perm)")
	assert.Contains(t, o.results[0].Stmts[1].Detail, "x has no result")
	// the report shows the reference
	o = run(t, pre+fmt.Sprintf("ck lstat(%q, perm == x.perm)", c))
	assert.Contains(t, o.out, "perm == x.perm")
	// text compares with text, regex from a variable works too
	o = run(t, "[s]\ne = environ(\"VERIFIEN_CMP\")\nf = environ(\"VERIFIEN_CMP\")\nck f.value == e.value")
	assert.True(t, o.results[0].Stmts[2].Pass, o.out)
}

func TestCompareItemTypeMismatchIsAnError(t *testing.T) {
	for name, c := range map[string]struct{ cfg, want string }{
		"perm with uid":     {"[a]\nx = lstat(\"/\")\nck lstat(\"/\", perm == x.uid)", "cannot be compared"},
		"uid with gid":      {"[a]\nx = lstat(\"/\")\nck lstat(\"/\", uid == x.gid)", "cannot be compared"},
		"operator for kind": {"[a]\nx = lstat(\"/\")\nck lstat(\"/\", perm > x.perm)", "not allowed"},
		"unknown variable":  {"[a]\nck lstat(\"/\", perm == nope.perm)", `unknown variable "nope"`},
		"later variable":    {"[a]\nck lstat(\"/\", perm == x.perm)\nx = lstat(\"/\")", `unknown variable "x"`},
		"unknown item":      {"[a]\nx = lstat(\"/\")\nck lstat(\"/\", perm == x.bogus)", `has no item "bogus"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := load(c.cfg)
			assert.ErrorContains(t, err, c.want)
		})
	}
	// size and a plain number are both numbers, so these may be compared
	mustLoad(t, "[a]\nx = lstat(\"/\")\nck lstat(\"/\", nlink >= x.size)")
}

func TestTargetChainOfVariables(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "real"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "l1")))
	require.NoError(t, os.Symlink(filepath.Join(dir, "l1"), filepath.Join(dir, "l2")))
	o := run(t, fmt.Sprintf(`[a]
a = lstat(%q)
b = lstat(a.link)
ck b.type == l
ck lstat(b.link, type == d)
`, filepath.Join(dir, "l2")))
	for i, st := range o.results[0].Stmts {
		assert.True(t, st.Pass, "statement %d %s: %s", i, st.Text, st.Detail)
	}
}

func TestTargetFromAFailedVariable(t *testing.T) {
	o := run(t, "[a]\nv = lstat(\"/nonexistent/x\")\nck lstat(v.link, type == d)")
	last := o.results[0].Stmts[1]
	assert.False(t, last.Pass)
	assert.Contains(t, last.Detail, "v has no result")
	// msg replaces that message, and ! turns the whole thing around
	o = run(t, "[a]\nv = lstat(\"/nonexistent/x\")\nck lstat(v.link, {msg \"no link\"}, type == d)")
	assert.Equal(t, "no link", o.results[0].Stmts[1].Detail)
	o = run(t, "[a]\nv = lstat(\"/nonexistent/x\")\nck ! lstat(v.link, type == d)")
	assert.True(t, o.results[0].Stmts[1].Pass)
	// a path that is not a link has link == "" and so a target of ""
	o = run(t, "[a]\nv = lstat(\"/\")\nck lstat(v.link, type == d)")
	assert.False(t, o.results[0].Stmts[1].Pass)
}

func TestTargetReferenceErrors(t *testing.T) {
	for name, c := range map[string]struct{ cfg, want string }{
		"unknown variable":     {"[a]\nck lstat(nope.link, type == d)", `unknown variable "nope"`},
		"assigned later":       {"[a]\nck lstat(v.link, type == d)\nv = lstat(\"/\")", `unknown variable "v"`},
		"unknown item":         {"[a]\nv = lstat(\"/\")\nck lstat(v.bogus, type == d)", `has no item "bogus"`},
		"item of its function": {"[a]\nv = environ(\"X\")\nck lstat(v.size, type == d)", `has no item "size"`},
		"own variable":         {"[a]\nv = lstat(v.link)", `unknown variable "v"`},
		"other section":        {"[a]\nv = lstat(\"/\")\n[b]\nck lstat(v.link, type == d)", `unknown variable "v"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := load(c.cfg)
			assert.ErrorContains(t, err, c.want)
		})
	}
	mustLoad(t, "[a]\nv = lstat(\"/\")\nif lstat(v.link, type == d)\nck ! lstat(v.name)\nw = lstat(v.link, {retry 1})")
}

// ---------------------------------------------------------------------------
// stat follows links, lstat does not
// ---------------------------------------------------------------------------

func TestStatAndLstatInAConfig(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "real"), 0o755))
	require.NoError(t, os.Chmod(filepath.Join(dir, "real"), 0o755))
	link := filepath.Join(dir, "ln")
	require.NoError(t, os.Symlink("real", link))
	allPass(t, fmt.Sprintf(`[a]
ck lstat(%[1]q, type == l, link == "real")
ck stat(%[1]q, type == d, perm == 0755)
ck ! stat(%[1]q, type == f)
ck ! lstat(%[1]q, type == d)
s = stat(%[1]q)
ck s.type == d
l = lstat(%[1]q)
ck l.link == "real"
ck stat(%[2]q, name == "real")
`, link, filepath.Join(dir, "real")))
	// an unusable link: stat fails (so ! stat passes), lstat sees the link
	dangling := filepath.Join(dir, "dangling")
	require.NoError(t, os.Symlink("nowhere", dangling))
	allPass(t, fmt.Sprintf("[a]\nck ! stat(%[1]q)\nck lstat(%[1]q, type == l)", dangling))
}

func TestStatHasNoLinkItem(t *testing.T) {
	for name, cfg := range map[string]string{
		"link test":  "[a]\nck stat(\"/\", link == \"x\")",
		"type l":     "[a]\nck stat(\"/\", type == l)",
		"on a var":   "[a]\ns = stat(\"/\")\nck s.link == \"x\"",
		"var target": "[a]\ns = stat(\"/\")\nck lstat(s.link)",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := load(cfg)
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), "link") || strings.Contains(err.Error(), "type"), err.Error())
		})
	}
	mustLoad(t, "[a]\nck lstat(\"/\", link == \"x\", type == l)")
}

// ---------------------------------------------------------------------------
// type == e (exists, any type) and type != e (does not exist)
// ---------------------------------------------------------------------------

func TestTypeEInAConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	missing := filepath.Join(dir, "missing")
	dangling := filepath.Join(dir, "dangling")
	require.NoError(t, os.Symlink(missing, dangling))
	for _, fn := range []string{"lstat", "stat"} {
		allPass(t, fmt.Sprintf(`[a]
ck %[1]s(%[2]q, type == e)
ck %[1]s(%[3]q, type != e)
ck ! %[1]s(%[3]q, type == e)
ck ! %[1]s(%[2]q, type != e)
ck %[1]s(%[2]q, type == e, type == f)
ck %[1]s(%[2]q, type == e, type != d)
if %[1]s(%[3]q, type != e)
`, fn, file, missing))
	}
	// the request: type == e and type == d together are not an error
	mustLoad(t, `[a]`+"\n"+`ck lstat("/", type == e, type == d)`+"\n"+`ck stat("/", type == d, type == e)`)
	// a missing path that is asked about only in other ways still fails
	o := run(t, fmt.Sprintf("[a]\nck lstat(%q, type == e)\nck lstat(%q, type == d)\nck lstat(%q, type != e, size == 0)", missing, missing, missing))
	for _, st := range o.results[0].Stmts {
		assert.False(t, st.Pass, st.Text)
	}
	assert.Contains(t, o.results[0].Stmts[0].Detail, "no such file")
	assert.Contains(t, o.results[0].Stmts[2].Detail, "did not report")
	// an existing file fails type != e, saying what it is
	o = run(t, fmt.Sprintf("[a]\nck lstat(%q, type != e)", file))
	assert.False(t, o.results[0].Stmts[0].Pass)
	assert.Contains(t, o.results[0].Stmts[0].Detail, "type != e is false (got f)")
	// a dangling link: lstat finds the link, stat finds nothing
	allPass(t, fmt.Sprintf("[a]\nck lstat(%[1]q, type == e, type == l)\nck stat(%[1]q, type != e)", dangling))
	// variables, and e as a test on a variable
	allPass(t, fmt.Sprintf("[a]\nv = lstat(%q)\nck v.type == e\nck v.type == f", file))
	// type != e does not run the call's retries when the answer is "missing"
	o = run(t, fmt.Sprintf("[a]\nck lstat(%q, {retry 3, sleep 5}, type != e)", missing))
	assert.True(t, o.results[0].Stmts[0].Pass, o.out)
	assert.Less(t, o.took.Seconds(), 3.0)
	// e is not a type of the other tests, and bad values are still errors
	_, _, err := load("[a]\nck lstat(\"/\", type == x)")
	assert.ErrorContains(t, err, "must be one of")
	_, _, err = load("[a]\nck lstat(\"/\", type > e)")
	assert.ErrorContains(t, err, "not allowed")
}
