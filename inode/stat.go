// Package inode holds the functions that look at inodes: files, directories,
// links, devices, pipes and sockets. Each file registers its function from init().
package inode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"verifien/spec"
)

// types are the values of the type item. They are the letters ls -l shows.
// e is not a type: type == e means the inode exists, whatever it is, and
// type != e means it does not exist (see look).
var types = []string{"f", "d", "l", "b", "c", "p", "s", "e"}

func init() {
	// lstat looks at the path itself (os.Lstat): a symbolic link is a link.
	// stat follows symbolic links (os.Stat): it describes what the link points to,
	// so it never reports type l and has no link item.
	spec.Register("lstat", newSpec(true))
	spec.Register("stat", newSpec(false))
}

func newSpec(isLstat bool) spec.FuncSpec {
	tests := map[string]spec.Field{
		// f file, d directory, l symbolic link (lstat only), b block special,
		// c character special, p fifo (named pipe), s socket
		"type":  {Kind: spec.KEnum, Enum: types, Ops: []string{"==", "!="}, Any: "e"},
		"size":  {Kind: spec.KSize}, // bytes; 0 to many T (powers of 1024)
		"nlink": {Kind: spec.KInt},  // number of hard links to the inode
		"uid":   {Kind: spec.KUID},  // owner, as a number or a login name
		"gid":   {Kind: spec.KGID},  // group, as a number or a group name
		// permission bits (07777), tested like find -perm: perm == 0644,
		// perm == "-0644" (all of these set), perm == "+0111" (any of these set)
		"perm": {Kind: spec.KPerm},
		"name": {Kind: spec.KString}, // base name; here mainly to show =~ and !~
	}
	run := stat
	if isLstat {
		run = lstat
		// where a symbolic link points, exactly as stored; "" if it is not a link
		tests["link"] = spec.Field{Kind: spec.KString}
	} else {
		// stat never sees a link, so l is not a type it can report
		tests["type"] = spec.Field{Kind: spec.KEnum, Enum: slices.DeleteFunc(slices.Clone(types), func(t string) bool { return t == "l" }), Ops: []string{"==", "!="}, Any: "e"}
	}
	// WantsTests: to see a type != e test, which asks whether the inode is missing
	return spec.FuncSpec{Run: run, Options: spec.StdOptions(), Tests: tests, WantsTests: true}
}

// info is what one Lstat or Stat (and, for a link, Readlink) learned.
type info struct {
	fi      os.FileInfo
	link    string
	missing bool // it does not exist, and a test (type != e) asked about that
}

func lstat(ctx context.Context, req spec.Request) (spec.Result, error) {
	return look(ctx, req, os.Lstat, true)
}

func stat(ctx context.Context, req spec.Request) (spec.Result, error) {
	return look(ctx, req, os.Stat, false)
}

func look(ctx context.Context, req spec.Request, get func(string) (os.FileInfo, error), withLink bool) (spec.Result, error) {
	// os.Lstat and os.Stat cannot be cancelled and can block for a long time on a
	// hung network mount. Attempt runs the call in its own goroutine, gives up on
	// it after req.Timeout, and retries errors, so there are at most req.Retry+1 tries.
	got, err := spec.Attempt(ctx, req, func() (info, error) {
		fi, err := get(req.Target)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && asksIfMissing(req.Tests) {
				return info{missing: true}, nil // an answer, so no retry
			}
			return info{}, err
		}
		in := info{fi: fi}
		if withLink && fi.Mode()&os.ModeSymlink != 0 {
			if in.link, err = os.Readlink(req.Target); err != nil {
				return info{}, err
			}
		}
		return in, nil
	})
	if err != nil {
		return nil, err
	}
	if got.missing {
		// Only type is reported, as "": type != e is true and every other test
		// fails with "did not report".
		return spec.Result{"type": ""}, nil
	}
	st, ok := got.fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("%s: no inode details on this system", req.Target)
	}
	res := spec.Result{
		"type":  fileType(got.fi.Mode()),
		"size":  got.fi.Size(),
		"nlink": int64(st.Nlink), // uint16 to uint64, depending on the system
		"uid":   int64(st.Uid),
		"gid":   int64(st.Gid),
		"perm":  int64(st.Mode) & 0o7777,
		"name":  filepath.Base(req.Target),
	}
	if withLink {
		res["link"] = got.link
	}
	return res, nil
}

// asksIfMissing reports whether a test is `type != e`: the call is asking
// whether the inode is absent, so absence is a result and not an error.
func asksIfMissing(tests []spec.TestRef) bool {
	for _, t := range tests {
		if t.Item == "type" && t.Op == "!=" && t.Want.Text() == "e" {
			return true
		}
	}
	return false
}

// fileType returns the letter ls -l would show for a mode.
func fileType(m os.FileMode) string {
	switch {
	case m.IsRegular():
		return "f"
	case m.IsDir():
		return "d"
	case m&os.ModeSymlink != 0:
		return "l"
	case m&os.ModeDevice != 0 && m&os.ModeCharDevice != 0:
		return "c"
	case m&os.ModeDevice != 0:
		return "b"
	case m&os.ModeNamedPipe != 0:
		return "p"
	case m&os.ModeSocket != 0:
		return "s"
	}
	return "other" // not testable: it is not one of the types
}
