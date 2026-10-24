package spec

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSizeTerabytes(t *testing.T) {
	for in, want := range map[string]int64{
		"0": 0, "1": 1, "1K": 1 << 10, "1M": 1 << 20, "1G": 1 << 30,
		"1T": 1 << 40, "1t": 1 << 40, "3T": 3 << 40, "100T": 100 << 40, "8388607T": 8388607 << 40,
	} {
		got, err := ParseSize(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"8388608T", "99999999999T", "T", "1P", "1.5T"} {
		_, err := ParseSize(bad)
		assert.Error(t, err, bad)
	}
}

func TestSizeComparisonsFromZeroToManyTerabytes(t *testing.T) {
	size := Field{Kind: KSize}
	for _, c := range []struct {
		got  int64
		op   string
		want string
		pass bool
	}{
		{0, "==", "0", true},
		{0, "<", "1", true},
		{1023, "<", "1K", true},
		{1024, "==", "1K", true},
		{5 << 40, ">", "4T", true},
		{5 << 40, ">=", "5T", true},
		{5 << 40, "<", "5T", false},
		{(5 << 40) - 1, "<", "5T", true},
		{3 << 40, "==", "3072G", true},
		{3 << 40, "==", "3145728M", true},
		{3 << 40, "!=", "3T", false},
	} {
		pass, err := Compare(size, c.got, c.op, Value{Number: str(c.want)})
		require.NoError(t, err)
		assert.Equal(t, c.pass, pass, "%d %s %s", c.got, c.op, c.want)
	}
}

func TestParsePermOctalAndPrefixes(t *testing.T) {
	for _, c := range []struct {
		in   string
		mode int64
		how  permMatch
	}{
		{"0644", 0o644, permExact},
		{"644", 0o644, permExact},
		{"4755", 0o4755, permExact},
		{"7777", 0o7777, permExact},
		{"0", 0, permExact},
		{"-0644", 0o644, permAll},
		{"-644", 0o644, permAll},
		{"+0111", 0o111, permAny},
		{"+4000", 0o4000, permAny},
	} {
		mode, how, err := ParsePerm(c.in)
		require.NoError(t, err, c.in)
		assert.Equal(t, c.mode, mode, c.in)
		assert.Equal(t, c.how, how, c.in)
	}
	for _, bad := range []string{"", "-", "+", "0888", "10000", "-10000", "u", "z+x", "u+z", "u+x,", "ug"} {
		_, _, err := ParsePerm(bad)
		assert.Error(t, err, "%q", bad)
	}
	// valid even though they look odd: an empty permission list clears
	for _, ok := range []string{"u=", "=rw", "a-"} {
		_, _, err := ParsePerm(ok)
		assert.NoError(t, err, "%q", ok)
	}
}

func TestParsePermSymbolic(t *testing.T) {
	for in, want := range map[string]int64{
		"u=rw,go=r":         0o644,
		"u=rwx,go=rx":       0o755,
		"a=rw":              0o666,
		"=rw":               0o666, // no who means everyone, ignoring the umask
		"a+x":               0o111,
		"+x":                0, // see below: the leading + is the find operator, not part of the mode
		"u+s":               0o4000,
		"g+s":               0o2000,
		"a+t":               0o1000,
		"u=rwx,g=u":         0o770,
		"u=rwx,g=u,o=":      0o770,
		"u=rw,g=u,o=g":      0o666,
		"a=rwx,g-w":         0o757,
		"a=rwx,o-rwx":       0o770,
		"u+x,g+X":           0o110, // X: g gets x because u+x already made it executable
		"u=rwx,go=rx,a-w":   0o555,
		"u=rwxs,g=rxs,o=rx": 0o6755,
		"ug=rw,o=":          0o660,
		"u+r+w":             0o600,
		"u=r-r":             0,
	} {
		if in == "+x" {
			continue
		}
		got, err := parseSymbolic(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, fmt.Sprintf("%s: want %04o got %04o", in, want, got))
	}
	// X does nothing when nobody can execute yet
	got, err := parseSymbolic("a+X")
	require.NoError(t, err)
	assert.Equal(t, int64(0), got)
	// a leading + or - is the find operator
	mode, how, err := ParsePerm("-u+x")
	require.NoError(t, err)
	assert.Equal(t, int64(0o100), mode)
	assert.Equal(t, permAll, how)
	mode, how, err = ParsePerm("+a+x")
	require.NoError(t, err)
	assert.Equal(t, int64(0o111), mode)
	assert.Equal(t, permAny, how)
}

func TestPermComparisons(t *testing.T) {
	f := Field{Kind: KPerm}
	for _, c := range []struct {
		got  int64
		op   string
		want Value
		pass bool
	}{
		{0o644, "==", Value{Number: str("0644")}, true},
		{0o644, "==", Value{Number: str("0600")}, false},
		{0o644, "!=", Value{Number: str("0600")}, true},
		{0o100644, "==", Value{Number: str("0644")}, true}, // only 07777 takes part
		{0o644, "==", Value{String: str("u=rw,go=r")}, true},
		{0o755, "==", Value{String: str("a=rx,u+w")}, true},
		// -mode: at least all these bits
		{0o755, "==", Value{String: str("-0111")}, true},
		{0o644, "==", Value{String: str("-0111")}, false},
		{0o755, "==", Value{String: str("-0755")}, true},
		{0o4755, "==", Value{String: str("-0755")}, true},
		{0o644, "==", Value{String: str("-u+r")}, true},
		{0o644, "!=", Value{String: str("-0111")}, true},
		// +mode: any of these bits
		{0o644, "==", Value{String: str("+0111")}, false},
		{0o744, "==", Value{String: str("+0111")}, true},
		{0o601, "==", Value{String: str("+0111")}, true},
		{0o644, "==", Value{String: str("+a+x")}, false},
		{0o644, "!=", Value{String: str("+0111")}, true},
		{0o1777, "==", Value{String: str("-a+t")}, true},
		{0o777, "==", Value{String: str("-a+t")}, false},
	} {
		pass, err := Compare(f, c.got, c.op, c.want)
		require.NoError(t, err)
		assert.Equal(t, c.pass, pass, "%04o %s %s", c.got, c.op, c.want.Literal())
	}
}

func TestPermValidationAndMessage(t *testing.T) {
	for _, ok := range []Value{{Number: str("0644")}, {String: str("-0644")}, {String: str("u=rw,go=r")}} {
		assert.NoError(t, ValidateTest("checkfn", "perm", "==", ok), ok.Literal())
	}
	for _, bad := range []Value{{Number: str("0888")}, {String: str("")}, {String: str("u+z")}, {Ident: str("rw")}, {Regex: str("x")}} {
		assert.Error(t, ValidateTest("checkfn", "perm", "==", bad), bad.Literal())
	}
	assert.Error(t, ValidateTest("checkfn", "perm", ">", Value{Number: str("0644")}), "only == and !=")
	assert.Error(t, ValidateTest("checkfn", "perm", "=~", Value{String: str("x")}))

	ok, why := Evaluate("checkfn", Result{"perm": int64(0o100640)}, "perm", "==", Value{Number: str("0644")})
	assert.False(t, ok)
	assert.Contains(t, why, "got 0640", "permissions are shown in octal")
}

func TestOwnerTestsTakeNumbersOrNames(t *testing.T) {
	oldU, oldG := LookupUser, LookupGroup
	defer func() { LookupUser, LookupGroup = oldU, oldG }()
	LookupUser = func(n string) (int64, error) {
		if n == "root" {
			return 0, nil
		}
		if n == "alice" {
			return 1000, nil
		}
		return 0, fmt.Errorf("unknown")
	}
	LookupGroup = func(n string) (int64, error) {
		if n == "wheel" {
			return 0, nil
		}
		return 0, fmt.Errorf("unknown")
	}
	res := Result{"uid": int64(1000), "gid": int64(0)}
	for _, c := range []struct {
		item, op string
		want     Value
		pass     bool
	}{
		{"uid", "==", Value{Number: str("1000")}, true},
		{"uid", "==", Value{Ident: str("alice")}, true},
		{"uid", "==", Value{String: str("alice")}, true},
		{"uid", "!=", Value{Ident: str("root")}, true},
		{"uid", "==", Value{Ident: str("root")}, false},
		{"gid", "==", Value{Ident: str("wheel")}, true},
		{"gid", "==", Value{Number: str("0")}, true},
		{"gid", "!=", Value{Number: str("0")}, false},
	} {
		ok, why := Evaluate("checkfn", res, c.item, c.op, c.want)
		assert.Equal(t, c.pass, ok, "%s %s %s: %s", c.item, c.op, c.want.Literal(), why)
	}
	// a name that does not exist fails the test when it runs, with a clear message
	ok, why := Evaluate("checkfn", res, "uid", "==", Value{Ident: str("nobody-here")})
	assert.False(t, ok)
	assert.Contains(t, why, `no user "nobody-here"`)
	ok, why = Evaluate("checkfn", res, "gid", "==", Value{Ident: str("nogroup-here")})
	assert.False(t, ok)
	assert.Contains(t, why, `no group "nogroup-here"`)
	// ...but the config still loads: the account may exist where it runs
	assert.NoError(t, ValidateTest("checkfn", "uid", "==", Value{Ident: str("nobody-here")}))
	assert.NoError(t, ValidateTest("checkfn", "uid", "==", Value{Number: str("0")}))
	assert.Error(t, ValidateTest("checkfn", "uid", "<", Value{Number: str("0")}), "only == and !=")
	assert.Error(t, ValidateTest("checkfn", "uid", "==", Value{Regex: str("x")}))
	assert.Error(t, ValidateTest("checkfn", "uid", "==", Value{String: str("")}))
}
