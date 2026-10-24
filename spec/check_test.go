package spec

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func str(s string) *string { return &s }

// These tests register their own function so they do not depend on the real ones.
func init() {
	Register("checkfn", FuncSpec{
		Run:     func(_ context.Context, req Request) (Result, error) { return Result{}, nil },
		Options: StdOptions(),
		Tests: map[string]Field{
			"kind":  {Kind: KEnum, Enum: []string{"f", "d"}, Ops: []string{"==", "!="}},
			"size":  {Kind: KSize},
			"count": {Kind: KInt},
			"name":  {Kind: KString},
			"uid":   {Kind: KUID},
			"gid":   {Kind: KGID},
			"perm":  {Kind: KPerm},
		},
	})
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"0": 0, "7": 7, "3K": 3 << 10, "3k": 3 << 10, "2M": 2 << 20, "10G": 10 << 30,
	} {
		got, err := ParseSize(in)
		if assert.NoError(t, err, in) {
			assert.Equal(t, want, got, in)
		}
	}
	for _, bad := range []string{"", "K", "1.5M", "abc", "3X"} {
		_, err := ParseSize(bad)
		assert.Error(t, err, "ParseSize(%q)", bad)
	}
}

func TestEvaluate(t *testing.T) {
	res := Result{"kind": "f", "size": int64(5 << 20), "count": int64(3), "name": "alpha.txt"}
	cases := []struct {
		item, op string
		want     Value
		pass     bool
	}{
		{"kind", "==", Value{Ident: str("f")}, true},
		{"kind", "!=", Value{Ident: str("d")}, true},
		{"kind", "==", Value{Ident: str("d")}, false},
		{"size", ">", Value{Number: str("3M")}, true},
		{"size", "<", Value{Number: str("3M")}, false},
		{"size", ">=", Value{Number: str("5M")}, true},
		{"count", "==", Value{Number: str("3")}, true},
		{"count", "<=", Value{Number: str("2")}, false},
		{"name", "==", Value{String: str("alpha.txt")}, true},
		{"name", "=~", Value{Regex: str(`^alpha\.`)}, true},
		{"name", "!~", Value{Regex: str(`^alpha\.`)}, false},
		{"name", "=~", Value{String: str(`txt$`)}, true},
		{"name", "<", Value{String: str("beta")}, true},
	}
	for _, c := range cases {
		pass, why := Evaluate("checkfn", res, c.item, c.op, c.want)
		assert.Equal(t, c.pass, pass, "%s %s %s (%s)", c.item, c.op, c.want.Literal(), why)
		if !pass {
			assert.NotEmpty(t, why, "%s %s %s: failed with no explanation", c.item, c.op, c.want.Literal())
		}
	}
}

func TestEvaluateMissingItem(t *testing.T) {
	pass, why := Evaluate("checkfn", Result{}, "size", ">", Value{Number: str("1")})
	assert.False(t, pass)
	assert.Contains(t, why, "did not report")
}

func TestValidateTest(t *testing.T) {
	ok := []struct {
		item, op string
		v        Value
	}{
		{"kind", "==", Value{Ident: str("f")}},
		{"size", ">", Value{Number: str("3M")}},
		{"count", ">=", Value{Number: str("3")}},
		{"name", "=~", Value{Regex: str("^a")}},
		{"name", "!~", Value{String: str("x")}},
	}
	for _, c := range ok {
		assert.NoError(t, ValidateTest("checkfn", c.item, c.op, c.v), "%s %s %s", c.item, c.op, c.v.Literal())
	}
	bad := []struct {
		item, op string
		v        Value
		wantSub  string
	}{
		{"nope", "==", Value{Ident: str("f")}, "has no test"},
		{"kind", "=~", Value{Regex: str("f")}, "not allowed"},
		{"kind", "<", Value{Ident: str("f")}, "not allowed"},
		{"kind", "==", Value{Ident: str("x")}, "must be one of"},
		{"size", "=~", Value{Regex: str("1")}, "not allowed"},
		{"size", ">", Value{Number: str("3X")}, "invalid size"},
		{"size", ">", Value{String: str("3")}, "expected a size"},
		{"count", ">", Value{Number: str("1.5")}, "invalid integer"},
		{"name", "=~", Value{Regex: str("(")}, "invalid regex"},
		{"name", "==", Value{Number: str("3")}, "expected a string"},
	}
	for _, c := range bad {
		err := ValidateTest("checkfn", c.item, c.op, c.v)
		assert.ErrorContains(t, err, c.wantSub, "%s %s %s", c.item, c.op, c.v.Literal())
	}
}

func TestCheckValueFloat(t *testing.T) {
	f := Field{Kind: KFloat}
	for _, good := range []string{"0", "0.5", "2", "1.25"} {
		assert.NoError(t, CheckValue(f, Value{Number: str(good)}), good)
	}
	for _, bad := range []string{"1s", "abc"} {
		assert.Error(t, CheckValue(f, Value{Number: str(bad)}), bad)
	}
	assert.Error(t, CheckValue(f, Value{String: str("1")}), "a quoted string must not be accepted for a decimal")
}

func TestRegisterPanics(t *testing.T) {
	mustPanic := func(name string, f func()) {
		t.Helper()
		assert.Panics(t, f, name)
	}
	run := func(_ context.Context, req Request) (Result, error) { return nil, nil }
	mustPanic("empty name", func() { Register("", FuncSpec{Run: run}) })
	mustPanic("nil Run", func() { Register("norun", FuncSpec{}) })
	mustPanic("duplicate", func() { Register("checkfn", FuncSpec{Run: run}) })

	act := func(_ context.Context, req ActionRequest) error { return nil }
	mustPanic("action empty", func() { RegisterAction("", ActionSpec{Run: act}) })
	mustPanic("action colon", func() { RegisterAction("a:b", ActionSpec{Run: act}) })
	mustPanic("action reserved", func() { RegisterAction("exit", ActionSpec{Run: act}) })
	mustPanic("action nil Run", func() { RegisterAction("norun", ActionSpec{}) })
}
