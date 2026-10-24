package spec

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
)

// This file is the one implementation of "item OP value" testing. The runner
// uses it for the tests inside a call and for checks on variables, and any
// function can call it too.

// Value is the right-hand side of a test, such as the 3G in "size > 3G" or the
// /^a/ in "name =~ /^a/". Exactly one field is set.
type Value struct {
	String *string // "text" or 'text', with the quotes removed
	Number *string // 3, 3K, 1.5G: the suffix is kept, see ParseSize
	Ident  *string // a bare word such as f
	Regex  *string // /pattern/, with the slashes removed
}

// Literal returns the value the way it is written in the config file.
func (v Value) Literal() string {
	switch {
	case v.String != nil:
		return strconv.Quote(*v.String)
	case v.Number != nil:
		return *v.Number
	case v.Ident != nil:
		return *v.Ident
	case v.Regex != nil:
		return "/" + *v.Regex + "/"
	}
	return "?"
}

// Text returns the value's text with quotes and slashes removed.
func (v Value) Text() string { return textValue(v) }

// Which operators each kind of item accepts. A Field's own Ops can narrow this
// further (as stat's "type" does).
var kindOps = map[Kind][]string{
	KInt:    {"==", "!=", "<", "<=", ">", ">="},
	KSize:   {"==", "!=", "<", "<=", ">", ">="},
	KString: {"==", "!=", "<", "<=", ">", ">=", "=~", "!~"}, // < > compare as text
	KEnum:   {"==", "!="},
	KUID:    {"==", "!="},
	KGID:    {"==", "!="},
	KPerm:   {"==", "!="},
}

var kindName = map[Kind]string{
	KInt:    "numbers",
	KSize:   "sizes",
	KString: "text",
	KEnum:   "enums",
	KFloat:  "decimal numbers",
	KUID:    "users",
	KGID:    "groups",
	KPerm:   "permissions",
}

// ValidateTest checks, before anything runs, that "item OP value" is allowed
// for the function fn: the item exists, the operator suits its kind, and the
// value has the right form. The error has no file position; callers add it.
func ValidateTest(fn, item, op string, v Value) error {
	fs, ok := Lookup(fn)
	if !ok {
		return fmt.Errorf("unknown function %q", fn)
	}
	f, ok := fs.TestField(item)
	if !ok {
		return fmt.Errorf("%s has no test %q", fn, item)
	}
	if f.Ops != nil && !slices.Contains(f.Ops, op) {
		return fmt.Errorf("operator %s not allowed for %s", op, item)
	}
	if !slices.Contains(kindOps[f.Kind], op) {
		return fmt.Errorf("operator %s not allowed for %s (%s supports %v)", op, item, kindName[f.Kind], kindOps[f.Kind])
	}
	if op == "=~" || op == "!~" {
		pat, ok := regexSource(v)
		if !ok {
			return fmt.Errorf("%s: expected a /regex/ or a quoted string", item)
		}
		if _, err := regexp.Compile(pat); err != nil {
			return fmt.Errorf("%s: invalid regex: %v", item, err)
		}
		return nil
	}
	if err := CheckValue(f, v); err != nil {
		return fmt.Errorf("%s: %w", item, err)
	}
	return nil
}

// Evaluate checks one "item OP value" test against what the function fn
// reported in res. It returns whether the test passed and, if not, why.
func Evaluate(fn string, res Result, item, op string, want Value) (bool, string) {
	fs, ok := Lookup(fn)
	if !ok {
		return false, fmt.Sprintf("unknown function %q", fn)
	}
	f, ok := fs.TestField(item)
	if !ok {
		return false, fmt.Sprintf("%s has no test %q", fn, item)
	}
	got, ok := res[item]
	if !ok {
		return false, fmt.Sprintf("%s did not report %q", fn, item)
	}
	pass, err := Compare(f, got, op, want)
	if err != nil {
		return false, fmt.Sprintf("%s: %v", item, err)
	}
	if !pass {
		return false, fmt.Sprintf("%s %s %s is false (got %s)", item, op, want.Literal(), showGot(f, got))
	}
	return true, ""
}

// FormatItem turns a reported value into text: for failure messages, and when a
// later call uses an item of a variable as its target ("lstat(var.link)").
// Permissions come out in octal, as ls and chmod show them (0640); sizes are
// plain byte counts; ids are numbers.
func FormatItem(f Field, got any) string {
	if n, ok := got.(int64); ok && f.Kind == KPerm {
		return fmt.Sprintf("%04o", n&permMask)
	}
	return fmt.Sprint(got)
}

func showGot(f Field, got any) string { return FormatItem(f, got) }

// Compare applies op to the value a function reported (got) and the value from
// the config file (want).
func Compare(f Field, got any, op string, want Value) (bool, error) {
	switch f.Kind {
	case KInt, KSize:
		g, ok := got.(int64)
		if !ok {
			return false, fmt.Errorf("function returned %T, want int64", got)
		}
		if want.Number == nil {
			return false, fmt.Errorf("expected a number")
		}
		var w int64
		var err error
		if f.Kind == KSize {
			w, err = ParseSize(*want.Number)
		} else {
			w, err = strconv.ParseInt(*want.Number, 10, 64)
		}
		if err != nil {
			return false, err
		}
		switch op {
		case "==":
			return g == w, nil
		case "!=":
			return g != w, nil
		case "<":
			return g < w, nil
		case "<=":
			return g <= w, nil
		case ">":
			return g > w, nil
		case ">=":
			return g >= w, nil
		}
	case KUID, KGID, KPerm:
		g, ok := got.(int64)
		if !ok {
			return false, fmt.Errorf("function returned %T, want int64", got)
		}
		if f.Kind == KPerm {
			return comparePerm(g, op, want)
		}
		w, err := ownerID(f.Kind, want)
		if err != nil {
			return false, err
		}
		switch op {
		case "==":
			return g == w, nil
		case "!=":
			return g != w, nil
		}
	case KString, KEnum:
		g, ok := got.(string)
		if !ok {
			return false, fmt.Errorf("function returned %T, want string", got)
		}
		w := textValue(want)
		if f.Kind == KEnum && f.Any != "" && w == f.Any {
			switch op {
			case "==":
				return g != "", nil
			case "!=":
				return g == "", nil
			}
		}
		switch op {
		case "==":
			return g == w, nil
		case "!=":
			return g != w, nil
		case "<":
			return g < w, nil
		case "<=":
			return g <= w, nil
		case ">":
			return g > w, nil
		case ">=":
			return g >= w, nil
		case "=~", "!~":
			re, err := regexp.Compile(w)
			if err != nil {
				return false, fmt.Errorf("invalid regex: %v", err)
			}
			if op == "=~" {
				return re.MatchString(g), nil
			}
			return !re.MatchString(g), nil
		}
	}
	return false, fmt.Errorf("operator %s not supported here", op)
}

// CheckValue checks that v has the form field f needs. The runner also uses it
// for option and [DEFAULTS] values.
func CheckValue(f Field, v Value) error {
	switch f.Kind {
	case KString:
		if v.String == nil && !(f.Loose && (v.Number != nil || v.Ident != nil)) {
			return fmt.Errorf("expected a string")
		}
	case KInt:
		if v.Number == nil {
			return fmt.Errorf("expected a number")
		}
		if _, err := strconv.Atoi(*v.Number); err != nil {
			return fmt.Errorf("invalid integer %q", *v.Number)
		}
	case KFloat:
		if v.Number == nil {
			return fmt.Errorf("expected a number such as 0.5")
		}
		if f, err := strconv.ParseFloat(*v.Number, 64); err != nil || f < 0 {
			return fmt.Errorf("invalid number %q", *v.Number)
		}
	case KSize:
		if v.Number == nil {
			return fmt.Errorf("expected a size")
		}
		if _, err := ParseSize(*v.Number); err != nil {
			return fmt.Errorf("invalid size %q", *v.Number)
		}
	case KUID, KGID:
		return checkOwnerValue(v)
	case KPerm:
		return checkPermValue(v)
	case KEnum:
		var s string
		switch {
		case v.Ident != nil:
			s = *v.Ident
		case v.String != nil:
			s = *v.String
		default:
			return fmt.Errorf("must be one of %v", f.Enum)
		}
		if !slices.Contains(f.Enum, s) {
			return fmt.Errorf("must be one of %v", f.Enum)
		}
	}
	return nil
}

// ParseSize converts "3", "3K", "3M", "3G", "3T" to bytes (powers of 1024:
// K = 1024, M = 1024*1024, and so on). A size that does not fit in 63 bits (more
// than 8388607T, about 8 EiB) is an error.
func ParseSize(s string) (int64, error) {
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K', 'k':
			mult, s = 1<<10, s[:n-1]
		case 'M', 'm':
			mult, s = 1<<20, s[:n-1]
		case 'G', 'g':
			mult, s = 1<<30, s[:n-1]
		case 'T', 't':
			mult, s = 1<<40, s[:n-1]
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if v > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return v * mult, nil
}

// regexSource returns the pattern from a /regex/ literal or a quoted string.
// Prefer /regex/: a quoted string needs backslashes doubled ("\\d+").
func regexSource(v Value) (string, bool) {
	switch {
	case v.Regex != nil:
		return *v.Regex, true
	case v.String != nil:
		return *v.String, true
	}
	return "", false
}

// textValue returns the text of a value for == != < > comparisons.
func textValue(v Value) string {
	switch {
	case v.Ident != nil:
		return *v.Ident
	case v.String != nil:
		return *v.String
	case v.Regex != nil:
		return *v.Regex
	case v.Number != nil:
		return *v.Number // only for a Loose item, as written
	}
	return ""
}

// ItemValue turns something a function reported into a Value, so that it can be
// the right-hand side of a test (ck b.perm == a.perm). Permissions become octal
// numbers (0640), other numbers plain decimals, text stays text.
func ItemValue(f Field, got any) Value {
	text := FormatItem(f, got)
	switch f.Kind {
	case KString:
		return Value{String: &text}
	case KEnum:
		return Value{Ident: &text}
	}
	return Value{Number: &text}
}

var numberText = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[A-Za-z]*$`)

// PrepareWant makes the value a test takes from a variable's item fit the item
// being tested, when the check runs. Text from a variable (say, the result of
// sed) that looks like a number (0640, 4096, 3K) is a number for a number, size,
// user, group or permission item; text such as root or -0640 stays text. The
// result is then checked as if it had been written in the config file, so
// "perm == a.value" fails with a clear message when a.value is not a permission.
func PrepareWant(fn, item, op string, want Value) (Value, error) {
	fs, ok := Lookup(fn)
	if !ok {
		return want, fmt.Errorf("unknown function %q", fn)
	}
	f, ok := fs.TestField(item)
	if !ok {
		return want, fmt.Errorf("%s has no test %q", fn, item)
	}
	switch f.Kind {
	case KInt, KSize, KUID, KGID, KPerm:
		if want.String != nil && numberText.MatchString(*want.String) {
			want = Value{Number: want.String}
		}
	}
	if op == "=~" || op == "!~" || f.Kind == KString {
		return want, nil
	}
	if err := CheckValue(f, want); err != nil {
		return want, fmt.Errorf("%s: %q is not usable here: %v", item, want.Text(), err)
	}
	return want, nil
}

// ValidateRef checks, before anything runs, that "item OP refFn.refItem" makes
// sense: both items exist, the operator suits the item, and the two items are
// the same kind of thing (a permission with a permission, a user with a user,
// text with text; sizes and plain numbers can be compared with each other).
func ValidateRef(fn, item, op, refFn, refItem string) error {
	fs, ok := Lookup(fn)
	if !ok {
		return fmt.Errorf("unknown function %q", fn)
	}
	f, ok := fs.TestField(item)
	if !ok {
		return fmt.Errorf("%s has no test %q", fn, item)
	}
	rs, ok := Lookup(refFn)
	if !ok {
		return fmt.Errorf("unknown function %q", refFn)
	}
	r, ok := rs.Tests[refItem]
	if !ok {
		return fmt.Errorf("%s has no item %q", refFn, refItem)
	}
	if f.Ops != nil && !slices.Contains(f.Ops, op) {
		return fmt.Errorf("operator %s not allowed for %s", op, item)
	}
	if !slices.Contains(kindOps[f.Kind], op) {
		return fmt.Errorf("operator %s not allowed for %s (%s supports %v)", op, item, kindName[f.Kind], kindOps[f.Kind])
	}
	numeric := func(k Kind) bool { return k == KInt || k == KSize }
	// Text can always stand in for anything (it is what sed produces, and it is
	// checked when the check runs), and anything can be written as text.
	if f.Kind != r.Kind && !(numeric(f.Kind) && numeric(r.Kind)) && f.Kind != KString && r.Kind != KString {
		return fmt.Errorf("%s is %s but %s is %s; they cannot be compared", item, kindName[f.Kind], refItem, kindName[r.Kind])
	}
	return nil
}
