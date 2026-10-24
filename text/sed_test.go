package text

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"verifien/spec"
)

func run(t *testing.T, target string, args ...string) string {
	t.Helper()
	fs, ok := spec.Lookup("sed")
	require.True(t, ok)
	require.NoError(t, fs.Args(args), "%q", args)
	res, err := fs.Run(context.Background(), spec.Request{Target: target, Args: args})
	require.NoError(t, err)
	return res["value"].(string)
}

func TestSubstitute(t *testing.T) {
	for name, c := range map[string]struct {
		in   string
		args []string
		want string
	}{
		"the request: add a leading slash": {"private/etc", []string{`s/^/\//`}, "/private/etc"},
		"first match only":                 {"aaa", []string{"s/a/b/"}, "baa"},
		"g":                                {"aaa", []string{"s/a/b/g"}, "bbb"},
		"nth":                              {"aaaa", []string{"s/a/b/2"}, "abaa"},
		"nth and g":                        {"aaaa", []string{"s/a/b/2g"}, "abbb"},
		"case-insensitive":                 {"Hello", []string{"s/h/J/i"}, "Jello"},
		"whole match with &":               {"abc", []string{"s/b/[&]/"}, "a[b]c"},
		"escaped &":                        {"abc", []string{`s/b/\&/`}, "a&c"},
		"groups":                           {"2026-10-14", []string{`s/(\d+)-(\d+)-(\d+)/\3.\2.\1/`}, "14.10.2026"},
		"other delimiter":                  {"/a/b", []string{"s|/a|/z|"}, "/z/b"},
		"delimiter in the expression":      {"a/b", []string{`s,a/b,x\,y,`}, "x,y"},
		"escaped slash in the expression":  {"a/b", []string{`s/a\/b/X/`}, "X"},
		"empty regex match at the start":   {"abc", []string{"s/x*/-/g"}, "-a-b-c-"},
		"delete with empty replacement":    {"a.b.c", []string{`s/\.//g`}, "abc"},
		"newline and tab":                  {"a b", []string{`s/ /\n/`, `s/b/\tb/`}, "a\n\tb"},
		"no match leaves the text":         {"abc", []string{"s/x/y/"}, "abc"},
		"unmatched group is empty":         {"ac", []string{`s/a(b)?c/[\1]/`}, "[]"},
		"in order, several arguments":      {"/etc", []string{"s/^/\\/private/", "s/etc/etc2/"}, "/private/etc2"},
		"several in one string":            {"abc", []string{"s/a/1/; s/b/2/\ns/c/3/"}, "123"},
		"y":                                {"hello", []string{"y/el/ip/"}, "hippo"},
		"y with an escaped delimiter":      {"a/b", []string{`y/\//-/`}, "a-b"},
		"s then y":                         {"0640", []string{"s/^0//", "y/0123456789/abcdefghij/"}, "gea"},
		"unicode":                          {"héllo", []string{"s/é/e/"}, "hello"},
		"permission text":                  {"0640", []string{`s/^/-/`}, "-0640"},
	} {
		t.Run(name, func(t *testing.T) { assert.Equal(t, c.want, run(t, c.in, c.args...)) })
	}
}

func TestBadCommands(t *testing.T) {
	fs, _ := spec.Lookup("sed")
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"none":              {nil, "needs at least one command"},
		"empty":             {[]string{""}, "is empty"},
		"unknown command":   {[]string{"d"}, "only s and y"},
		"no delimiter":      {[]string{"s"}, "needs a delimiter"},
		"bad delimiter":     {[]string{`s\a\b\`}, "bad delimiter"},
		"alnum delimiter":   {[]string{"sxaxbx"}, "bad delimiter"},
		"unterminated":      {[]string{"s/a/b"}, "unterminated"},
		"unterminated y":    {[]string{"y/a/"}, "unterminated"},
		"bad regex":         {[]string{"s/(/x/"}, "bad regular expression"},
		"bad flag":          {[]string{"s/a/b/w"}, "unsupported flag"},
		"missing group":     {[]string{`s/a/\1/`}, "uses \\1"},
		"y lengths":         {[]string{"y/ab/c/"}, "same length"},
		"junk after":        {[]string{"s/a/b/ x"}, "unknown command"},
		"second one is bad": {[]string{"s/a/b/", "s/a"}, "unterminated"},
	} {
		t.Run(name, func(t *testing.T) { assert.ErrorContains(t, fs.Args(c.args), c.want) })
	}
}
