package inode

import (
	"context"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"verifien/spec"
)

func req(path string) spec.Request {
	return spec.Request{Target: path, Timeout: time.Second}
}

func TestStatFileDirLink(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(file, []byte("hello"), 0o644))
	link := filepath.Join(dir, "ln")
	if err := os.Symlink(file, link); err != nil {
		t.Skip("symlinks not available:", err)
	}
	cases := []struct {
		path, typ string
		size      int64
		name      string
	}{
		{file, "f", 5, "a.txt"},
		{dir, "d", -1, filepath.Base(dir)},
		{link, "l", -1, "ln"},
	}
	for _, c := range cases {
		res, err := lstat(context.Background(), req(c.path))
		require.NoError(t, err, c.path)
		assert.Equal(t, c.typ, res["type"], c.path)
		assert.Equal(t, c.name, res["name"], c.path)
		if c.size >= 0 {
			assert.Equal(t, c.size, res["size"], c.path)
		}
	}
}

func TestStatMissingFileIsAnError(t *testing.T) {
	_, err := lstat(context.Background(), req(filepath.Join(t.TempDir(), "nope")))
	assert.Error(t, err)
}

// Retry: the file appears while stat is still retrying, so it succeeds.
func TestStatRetriesUntilTheFileAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late")
	go func() {
		time.Sleep(250 * time.Millisecond)
		os.WriteFile(path, []byte("x"), 0o644)
	}()
	r := spec.Request{Target: path, Timeout: time.Second, Retry: 5, Sleep: 100 * time.Millisecond}
	res, err := lstat(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, "f", res["type"])
}

func TestStatWithoutRetryFailsAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late")
	start := time.Now()
	_, err := lstat(context.Background(), spec.Request{Target: path, Timeout: time.Second, Sleep: time.Second})
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}

// ---------------------------------------------------------------------------
// type letters, nlink, link, uid, gid, perm, big sizes
// ---------------------------------------------------------------------------

func statOf(t *testing.T, path string) spec.Result {
	t.Helper()
	res, err := lstat(context.Background(), req(path))
	require.NoError(t, err, path)
	return res
}

func TestStatSpecialFileTypes(t *testing.T) {
	dir := t.TempDir()

	fifo := filepath.Join(dir, "fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	assert.Equal(t, "p", statOf(t, fifo)["type"], "fifo")

	// a unix socket: the socket file stays after the listener closes
	sock := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Log("cannot make a unix socket here:", err)
	} else {
		defer ln.Close()
		assert.Equal(t, "s", statOf(t, sock)["type"], "socket")
	}

	assert.Equal(t, "c", statOf(t, "/dev/null")["type"], "character special")

	// a block device exists only on some machines (often not in containers)
	var block string
	entries, _ := os.ReadDir("/dev")
	for _, e := range entries {
		if fi, err := os.Lstat(filepath.Join("/dev", e.Name())); err == nil &&
			fi.Mode()&os.ModeDevice != 0 && fi.Mode()&os.ModeCharDevice == 0 {
			block = filepath.Join("/dev", e.Name())
			break
		}
	}
	if block == "" {
		t.Log("no block device in /dev here; the b letter is covered by TestFileTypeLetters")
	} else {
		assert.Equal(t, "b", statOf(t, block)["type"], "block special")
	}
}

// fileType maps modes without needing the real thing, so b is tested everywhere.
func TestFileTypeLetters(t *testing.T) {
	for _, c := range []struct {
		mode os.FileMode
		want string
	}{
		{0o644, "f"},
		{os.ModeDir | 0o755, "d"},
		{os.ModeSymlink | 0o777, "l"},
		{os.ModeDevice | 0o660, "b"},
		{os.ModeDevice | os.ModeCharDevice | 0o660, "c"},
		{os.ModeNamedPipe | 0o600, "p"},
		{os.ModeSocket | 0o600, "s"},
	} {
		assert.Equal(t, c.want, fileType(c.mode), c.mode.String())
	}
}

func TestStatNlinkAndLink(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	assert.EqualValues(t, 1, statOf(t, file)["nlink"])
	require.NoError(t, os.Link(file, filepath.Join(dir, "b")))
	require.NoError(t, os.Link(file, filepath.Join(dir, "c")))
	assert.EqualValues(t, 3, statOf(t, file)["nlink"])

	// link is the target exactly as stored, relative or absolute, even if dangling
	require.NoError(t, os.Symlink("a", filepath.Join(dir, "rel")))
	require.NoError(t, os.Symlink(file, filepath.Join(dir, "abs")))
	require.NoError(t, os.Symlink("/no/such/place", filepath.Join(dir, "dangling")))
	assert.Equal(t, "a", statOf(t, filepath.Join(dir, "rel"))["link"])
	assert.Equal(t, file, statOf(t, filepath.Join(dir, "abs"))["link"])
	assert.Equal(t, "/no/such/place", statOf(t, filepath.Join(dir, "dangling"))["link"])
	assert.Equal(t, "l", statOf(t, filepath.Join(dir, "dangling"))["type"])
	// not a link: empty
	assert.Equal(t, "", statOf(t, file)["link"])
}

func TestStatOwnerAndGroup(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	res := statOf(t, file)
	assert.EqualValues(t, os.Getuid(), res["uid"])
	assert.EqualValues(t, os.Getgid(), res["gid"])

	// by name and by number, through the shared test code
	u, err := user.Current()
	require.NoError(t, err)
	ok, why := spec.Evaluate("lstat", res, "uid", "==", spec.Value{Number: strp(u.Uid)})
	assert.True(t, ok, why)
	ok, why = spec.Evaluate("lstat", res, "uid", "==", spec.Value{Ident: strp(u.Username)})
	assert.True(t, ok, why)
	ok, why = spec.Evaluate("lstat", res, "uid", "==", spec.Value{String: strp(u.Username)})
	assert.True(t, ok, why)
	if g, err := user.LookupGroupId(u.Gid); err == nil {
		ok, why = spec.Evaluate("lstat", res, "gid", "==", spec.Value{Ident: strp(g.Name)})
		assert.True(t, ok, why)
	}
	// root's files: uid 0 is root by name
	if r, err := user.Lookup("root"); err == nil {
		rootRes := statOf(t, "/")
		ok, _ = spec.Evaluate("lstat", rootRes, "uid", "==", spec.Value{Ident: strp("root")})
		assert.Equal(t, rootRes["uid"] == int64(mustAtoi(t, r.Uid)), ok)
	}
}

func strp(s string) *string { return &s }

func mustAtoi(t *testing.T, s string) int {
	n, err := strconv.Atoi(s)
	require.NoError(t, err)
	return n
}

func TestStatPerm(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	for _, mode := range []os.FileMode{0o644, 0o755, 0o600, 0o640, 0o4755, 0o2755, 0o1777, 0o7777} {
		require.NoError(t, os.Chmod(file, mode&0o777|modeBits(mode)))
		got := statOf(t, file)["perm"].(int64)
		assert.Equal(t, int64(mode), got, "%04o", mode)
	}

	// the find -perm forms, through the shared test code
	require.NoError(t, os.Chmod(file, 0o755))
	res := statOf(t, file)
	for _, c := range []struct {
		op   string
		want spec.Value
		pass bool
	}{
		{"==", spec.Value{Number: strp("0755")}, true},
		{"==", spec.Value{Number: strp("0644")}, false},
		{"==", spec.Value{String: strp("u=rwx,go=rx")}, true},
		{"==", spec.Value{String: strp("-0111")}, true},  // at least all x bits
		{"==", spec.Value{String: strp("-0222")}, false}, // group/other cannot write
		{"==", spec.Value{String: strp("+0002")}, false}, // nobody else writes
		{"==", spec.Value{String: strp("+0022")}, false},
		{"==", spec.Value{String: strp("+0200")}, true},
		{"!=", spec.Value{String: strp("+0022")}, true},
	} {
		ok, why := spec.Evaluate("lstat", res, "perm", c.op, c.want)
		assert.Equal(t, c.pass, ok, "perm %s %s: %s", c.op, c.want.Literal(), why)
	}
}

// modeBits turns the 07000 bits of a numeric mode into os.FileMode flags, which
// is what os.Chmod wants.
func modeBits(m os.FileMode) os.FileMode {
	var out os.FileMode
	if m&0o4000 != 0 {
		out |= os.ModeSetuid
	}
	if m&0o2000 != 0 {
		out |= os.ModeSetgid
	}
	if m&0o1000 != 0 {
		out |= os.ModeSticky
	}
	return out
}

func TestStatBigSizes(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		size int64
		eq   string
	}{
		{"zero", 0, "0"},
		{"one-gig", 1 << 30, "1G"},
		{"two-tera", 2 << 40, "2T"},
	}
	for _, c := range cases {
		path := filepath.Join(dir, c.name)
		f, err := os.Create(path)
		require.NoError(t, err)
		if err := f.Truncate(c.size); err != nil { // a sparse file: no disk is used
			f.Close()
			t.Skipf("this file system cannot make a %s sparse file: %v", c.eq, err)
		}
		f.Close()
		res := statOf(t, path)
		assert.Equal(t, c.size, res["size"], c.name)
		for _, op := range []string{"==", ">=", "<="} {
			ok, why := spec.Evaluate("lstat", res, "size", op, spec.Value{Number: strp(c.eq)})
			assert.True(t, ok, "size %s %s: %s", op, c.eq, why)
		}
	}
	res := statOf(t, filepath.Join(dir, "two-tera"))
	ok, _ := spec.Evaluate("lstat", res, "size", ">", spec.Value{Number: strp("1T")})
	assert.True(t, ok)
	ok, _ = spec.Evaluate("lstat", res, "size", "<", spec.Value{Number: strp("3T")})
	assert.True(t, ok)
	ok, _ = spec.Evaluate("lstat", res, "size", ">", spec.Value{Number: strp("2T")})
	assert.False(t, ok)
}

func TestLstatAndStatTestsAreRegistered(t *testing.T) {
	ls, ok := spec.Lookup("lstat")
	require.True(t, ok)
	for _, item := range []string{"type", "size", "nlink", "link", "uid", "gid", "perm", "name"} {
		assert.Contains(t, ls.Tests, item)
	}
	assert.ElementsMatch(t, []string{"f", "d", "l", "b", "c", "p", "s", "e"}, ls.Tests["type"].Enum)

	// stat follows links, so it has no link item and cannot report type l
	st, ok := spec.Lookup("stat")
	require.True(t, ok)
	for _, item := range []string{"type", "size", "nlink", "uid", "gid", "perm", "name"} {
		assert.Contains(t, st.Tests, item)
	}
	assert.NotContains(t, st.Tests, "link")
	assert.ElementsMatch(t, []string{"f", "d", "b", "c", "p", "s", "e"}, st.Tests["type"].Enum)
	assert.Len(t, st.Tests, 7)
	assert.Len(t, ls.Tests, 8)
}

func TestStatFollowsLinksAndLstatDoesNot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "target.txt")
	require.NoError(t, os.WriteFile(file, []byte("hello"), 0o640))
	require.NoError(t, os.Chmod(file, 0o640))
	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))
	fileLink, dirLink := filepath.Join(dir, "flink"), filepath.Join(dir, "dlink")
	require.NoError(t, os.Symlink(file, fileLink))
	require.NoError(t, os.Symlink("sub", dirLink)) // a relative link
	chain := filepath.Join(dir, "chain")
	require.NoError(t, os.Symlink(fileLink, chain)) // a link to a link

	for _, c := range []struct {
		path, typ string
		perm      int64
	}{{fileLink, "f", 0o640}, {dirLink, "d", 0o755}, {chain, "f", 0o640}, {file, "f", 0o640}} {
		res, err := stat(context.Background(), req(c.path))
		require.NoError(t, err, c.path)
		assert.Equal(t, c.typ, res["type"], c.path)
		assert.EqualValues(t, c.perm, res["perm"], c.path)
		assert.NotContains(t, res, "link", "stat reports no link item")
		assert.Equal(t, filepath.Base(c.path), res["name"], "the name is the one asked for, not the target's")
	}
	res, _ := stat(context.Background(), req(fileLink))
	assert.EqualValues(t, 5, res["size"], "the size of the file, not of the link")

	// lstat on the same paths sees the links
	res, err := lstat(context.Background(), req(fileLink))
	require.NoError(t, err)
	assert.Equal(t, "l", res["type"])
	assert.Equal(t, file, res["link"])
	res, _ = lstat(context.Background(), req(dirLink))
	assert.Equal(t, "sub", res["link"])
	assert.NotEqual(t, int64(5), res["size"])

	// a link whose target is missing: stat fails, lstat still sees the link
	dangling := filepath.Join(dir, "dangling")
	require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), dangling))
	_, err = stat(context.Background(), req(dangling))
	assert.ErrorIs(t, err, os.ErrNotExist)
	res, err = lstat(context.Background(), req(dangling))
	require.NoError(t, err)
	assert.Equal(t, "l", res["type"])

	// and a loop is an error for stat
	loop := filepath.Join(dir, "loop")
	require.NoError(t, os.Symlink(loop, loop))
	_, err = stat(context.Background(), req(loop))
	assert.Error(t, err)
}

func TestStatRetriesAndTimesOutLikeLstat(t *testing.T) {
	_, err := stat(context.Background(), spec.Request{Target: filepath.Join(t.TempDir(), "nope"), Timeout: time.Second, Retry: 2})
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func notE() []spec.TestRef {
	e := "e"
	return []spec.TestRef{{Item: "type", Op: "!=", Want: spec.Value{Ident: &e}}}
}

// type == e: it exists, whatever it is. type != e: it does not exist.
func TestTypeEMeansExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	link := filepath.Join(dir, "ln")
	require.NoError(t, os.Symlink(file, link))
	dangling := filepath.Join(dir, "dangling")
	require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), dangling))
	missing := filepath.Join(dir, "missing")
	e := spec.Value{Ident: strp("e")}

	for _, fn := range []string{"lstat", "stat"} {
		run := func(path string, tests ...spec.TestRef) spec.Result {
			fs, _ := spec.Lookup(fn)
			res, err := fs.Run(context.Background(), spec.Request{Target: path, Timeout: time.Second, Tests: tests})
			require.NoError(t, err, "%s %s", fn, path)
			return res
		}
		eval := func(res spec.Result, op string) bool {
			ok, _ := spec.Evaluate(fn, res, "type", op, e)
			return ok
		}
		for _, path := range []string{file, dir, link} {
			res := run(path, notE()...)
			assert.True(t, eval(res, "=="), "%s %s exists", fn, path)
			assert.False(t, eval(res, "!="), "%s %s exists, so != e is false", fn, path)
		}
		// absent: with a type != e test it is a result, and says "not there"
		res := run(missing, notE()...)
		assert.Equal(t, "", res["type"])
		assert.True(t, eval(res, "!="), fn)
		assert.False(t, eval(res, "=="), fn)
		assert.NotContains(t, res, "size")
		ok, why := spec.Evaluate(fn, res, "size", "==", spec.Value{Number: strp("0")})
		assert.False(t, ok)
		assert.Contains(t, why, "did not report")
	}
	// without a type != e test, absence is still an error (and is retried)
	_, err := lstat(context.Background(), req(missing))
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = stat(context.Background(), spec.Request{Target: missing, Timeout: time.Second,
		Tests: []spec.TestRef{{Item: "type", Op: "==", Want: e}}})
	assert.ErrorIs(t, err, os.ErrNotExist)
	// a dangling link: lstat finds the link, stat finds nothing there
	res, err := lstat(context.Background(), spec.Request{Target: dangling, Timeout: time.Second, Tests: notE()})
	require.NoError(t, err)
	assert.Equal(t, "l", res["type"])
	res, err = stat(context.Background(), spec.Request{Target: dangling, Timeout: time.Second, Tests: notE()})
	require.NoError(t, err)
	assert.Equal(t, "", res["type"])
}
