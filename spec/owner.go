package spec

import (
	"fmt"
	"os/user"
	"strconv"
)

// User and group items (stat's uid and gid) are reported as numbers (int64) but
// may be tested against a number or a name: uid == 0, uid == root, uid == "root".

// LookupUser turns a login name into a user id, LookupGroup a group name into a
// group id. They are variables so tests can replace them.
var (
	LookupUser = func(name string) (int64, error) {
		u, err := user.Lookup(name)
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(u.Uid, 10, 64)
	}
	LookupGroup = func(name string) (int64, error) {
		g, err := user.LookupGroup(name)
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(g.Gid, 10, 64)
	}
)

// ownerID returns the id a uid/gid test value stands for. A number is the id; a
// bare word or quoted string is a name, looked up when the test runs (so a config
// shared between machines does not fail to load on one that lacks the account).
func ownerID(kind Kind, v Value) (int64, error) {
	if v.Number != nil {
		id, err := strconv.ParseInt(*v.Number, 10, 64)
		if err != nil || id < 0 {
			return 0, fmt.Errorf("invalid id %q", *v.Number)
		}
		return id, nil
	}
	var name string
	switch {
	case v.Ident != nil:
		name = *v.Ident
	case v.String != nil:
		name = *v.String
	default:
		return 0, fmt.Errorf("expected a number or a name")
	}
	if kind == KGID {
		id, err := LookupGroup(name)
		if err != nil {
			return 0, fmt.Errorf("no group %q", name)
		}
		return id, nil
	}
	id, err := LookupUser(name)
	if err != nil {
		return 0, fmt.Errorf("no user %q", name)
	}
	return id, nil
}

// checkOwnerValue is the form check done before anything runs: a number, a bare
// word or a non-empty quoted string. The name is not looked up yet.
func checkOwnerValue(v Value) error {
	switch {
	case v.Number != nil:
		if id, err := strconv.ParseInt(*v.Number, 10, 64); err != nil || id < 0 {
			return fmt.Errorf("invalid id %q", *v.Number)
		}
	case v.Ident != nil:
	case v.String != nil && *v.String != "":
	default:
		return fmt.Errorf("expected a number or a name")
	}
	return nil
}
