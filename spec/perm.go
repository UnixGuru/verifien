package spec

import (
	"fmt"
	"strconv"
	"strings"
)

// Permission tests work like find(1)'s -perm:
//
//	perm == 0644      the mode bits are exactly these
//	perm == "-0644"   at least all of these bits are set
//	perm == "+0111"   any of these bits is set
//
// The mode is octal (only bits 07777 take part) or symbolic as for chmod(1)
// ("u=rw,go=r", "a+x"), starting from zero and ignoring the umask. A leading - or
// + is the find operator, so a symbolic mode cannot start with a dash. Because of
// the - and +, a mode that has one is written as a quoted string. != negates.

const permMask = 0o7777

type permMatch int

const (
	permExact permMatch = iota // bits == mode
	permAll                    // bits & mode == mode
	permAny                    // bits & mode != 0
)

// ParsePerm parses the text after "perm ==": [-|+]mode.
func ParsePerm(s string) (mode int64, how permMatch, err error) {
	how = permExact
	switch {
	case strings.HasPrefix(s, "-"):
		how, s = permAll, s[1:]
	case strings.HasPrefix(s, "+"):
		how, s = permAny, s[1:]
	}
	if s == "" {
		return 0, 0, fmt.Errorf("empty mode")
	}
	if isDigits(s) {
		n, err := strconv.ParseUint(s, 8, 32)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid octal mode %q", s)
		}
		if n > permMask {
			return 0, 0, fmt.Errorf("octal mode %q is above 7777", s)
		}
		return int64(n), how, nil
	}
	m, err := parseSymbolic(s)
	if err != nil {
		return 0, 0, err
	}
	return m, how, nil
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// permText returns the text of a perm test value, which is an octal number such
// as 0644 or a quoted string such as "-0644" or "u=rw,go=r".
func permText(v Value) (string, bool) {
	switch {
	case v.Number != nil:
		return *v.Number, true
	case v.String != nil:
		return *v.String, true
	}
	return "", false
}

func checkPermValue(v Value) error {
	s, ok := permText(v)
	if !ok {
		return fmt.Errorf(`expected an octal mode such as 0644, or a quoted mode such as "-0644" or "u=rw,go=r"`)
	}
	_, _, err := ParsePerm(s)
	return err
}

func comparePerm(got int64, op string, want Value) (bool, error) {
	s, ok := permText(want)
	if !ok {
		return false, fmt.Errorf("expected a mode")
	}
	mode, how, err := ParsePerm(s)
	if err != nil {
		return false, err
	}
	bits := got & permMask
	var match bool
	switch how {
	case permExact:
		match = bits == mode
	case permAll:
		match = bits&mode == mode
	case permAny:
		match = bits&mode != 0
	}
	switch op {
	case "==":
		return match, nil
	case "!=":
		return !match, nil
	}
	return false, fmt.Errorf("operator %s not supported here", op)
}

// parseSymbolic evaluates a chmod symbolic mode against a starting mode of zero
// (no umask is applied). A clause is [ugoa]*([-+=][rwxXstugo]*)+ and clauses are
// separated by commas. With no who letters the clause applies to everyone.
func parseSymbolic(s string) (int64, error) {
	var mode int64
	for _, clause := range strings.Split(s, ",") {
		i := 0
		var who int64 // 4 = user, 2 = group, 1 = other
		for ; i < len(clause); i++ {
			switch clause[i] {
			case 'u':
				who |= 4
			case 'g':
				who |= 2
			case 'o':
				who |= 1
			case 'a':
				who |= 7
			default:
				goto op
			}
		}
	op:
		if i == len(clause) || !strings.ContainsRune("+-=", rune(clause[i])) {
			return 0, fmt.Errorf("invalid symbolic mode %q", s)
		}
		explicit := who != 0
		if !explicit {
			who = 7
		}
		for i < len(clause) {
			opChar := clause[i]
			i++
			var bits int64
			for ; i < len(clause) && !strings.ContainsRune("+-=", rune(clause[i])); i++ {
				b, err := permLetter(clause[i], who, mode)
				if err != nil {
					return 0, fmt.Errorf("invalid symbolic mode %q: %v", s, err)
				}
				bits |= b
			}
			switch opChar {
			case '+':
				mode |= bits
			case '-':
				mode &^= bits
			case '=':
				mode &^= whoMask(who, !explicit || who == 7)
				mode |= bits
			}
		}
	}
	return mode, nil
}

// whoMask is every bit '=' clears for the classes in who: their rwx bits, setuid
// for the user, setgid for the group, and the sticky bit when it was a=.
func whoMask(who int64, all bool) int64 {
	var m int64
	if who&4 != 0 {
		m |= 0o700 | 0o4000
	}
	if who&2 != 0 {
		m |= 0o070 | 0o2000
	}
	if who&1 != 0 {
		m |= 0o007
	}
	if all {
		m |= 0o1000
	}
	return m
}

// permLetter returns the bits one permission letter stands for, for the classes
// in who, given the mode so far (for X, u, g and o).
func permLetter(c byte, who, mode int64) (int64, error) {
	spread := func(rwx int64) int64 { // rwx is 0..7; place it in each selected class
		var b int64
		if who&4 != 0 {
			b |= rwx << 6
		}
		if who&2 != 0 {
			b |= rwx << 3
		}
		if who&1 != 0 {
			b |= rwx
		}
		return b
	}
	switch c {
	case 'r':
		return spread(4), nil
	case 'w':
		return spread(2), nil
	case 'x':
		return spread(1), nil
	case 'X': // execute only if somebody can already execute
		if mode&0o111 != 0 {
			return spread(1), nil
		}
		return 0, nil
	case 's':
		var b int64
		if who&4 != 0 {
			b |= 0o4000
		}
		if who&2 != 0 {
			b |= 0o2000
		}
		return b, nil
	case 't':
		return 0o1000, nil
	case 'u':
		return spread((mode >> 6) & 7), nil
	case 'g':
		return spread((mode >> 3) & 7), nil
	case 'o':
		return spread(mode & 7), nil
	}
	return 0, fmt.Errorf("unknown permission %q", string(c))
}
