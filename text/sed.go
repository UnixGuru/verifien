// Package text holds the functions that work on strings. Each file registers its
// function from init().
package text

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"verifien/spec"
)

func init() {
	spec.Register("sed", spec.FuncSpec{
		Run: sed,
		// The standard options are accepted but sed never waits, so only msg does anything.
		Options: spec.StdOptions(),
		Tests:   map[string]spec.Field{"value": {Kind: spec.KString}}, // the changed text
		Args:    checkScripts,
		// the text to change may be any item of a variable: a number, a permission, ...
		TextInput: true,
	})
}

// sed("text", "s/a/b/", "s/c/d/g", ...) applies the commands, in order, to the
// target text and reports the result as `value`. The commands are those of sed
// that change text: s and y (see parse). Several commands may also share one
// string, separated by ; or a newline.
func sed(ctx context.Context, req spec.Request) (spec.Result, error) {
	cmds, err := parseAll(req.Args)
	if err != nil {
		return nil, err
	}
	out := req.Target
	for _, c := range cmds {
		out = c.apply(out)
	}
	return spec.Result{"value": out}, nil
}

// checkScripts is run when the config is loaded, so a bad command is reported
// with the line it is on.
func checkScripts(args []string) error {
	if len(args) == 0 {
		return errors.New(`needs at least one command after the text, such as "s/old/new/"`)
	}
	_, err := parseAll(args)
	return err
}

func parseAll(args []string) ([]command, error) {
	var all []command
	for _, a := range args {
		cmds, err := parse(a)
		if err != nil {
			return nil, fmt.Errorf("command %q: %v", a, err)
		}
		all = append(all, cmds...)
	}
	return all, nil
}

type command interface{ apply(string) string }

// parse reads one string of commands: s/regex/replacement/flags and y/from/to/,
// separated by ; or newlines. The delimiter after s or y can be any character
// except a backslash, newline, letter or digit (s|a|b|, s,a,b,); inside, \ followed by
// the delimiter means the delimiter itself.
func parse(s string) ([]command, error) {
	var cmds []command
	i := 0
	skip := func() {
		for i < len(s) && strings.ContainsRune(" \t\n;", rune(s[i])) {
			i++
		}
	}
	for skip(); i < len(s); skip() {
		kind := s[i]
		if kind != 's' && kind != 'y' {
			return nil, fmt.Errorf("unknown command %q (only s and y are supported)", string(kind))
		}
		i++
		if i >= len(s) {
			return nil, fmt.Errorf("%c needs a delimiter, as in %c/old/new/", kind, kind)
		}
		delim, w := utf8.DecodeRuneInString(s[i:])
		if delim == '\\' || delim == '\n' || delim == utf8.RuneError || isAlnum(delim) {
			return nil, fmt.Errorf("%c: bad delimiter %q", kind, string(delim))
		}
		i += w
		first, n, ok := field(s[i:], delim)
		if !ok {
			return nil, fmt.Errorf("%c: unterminated command (missing a %q)", kind, string(delim))
		}
		i += n
		second, n, ok := field(s[i:], delim)
		if !ok {
			return nil, fmt.Errorf("%c: unterminated command (missing a %q)", kind, string(delim))
		}
		i += n
		if kind == 'y' {
			c, err := newTransliterate(first, second, delim)
			if err != nil {
				return nil, err
			}
			cmds = append(cmds, c)
			continue
		}
		start := i
		for i < len(s) && !strings.ContainsRune(" \t\n;", rune(s[i])) {
			i++
		}
		c, err := newSubst(first, second, s[start:i], delim)
		if err != nil {
			return nil, err
		}
		cmds = append(cmds, c)
	}
	if len(cmds) == 0 {
		return nil, errors.New("is empty")
	}
	return cmds, nil
}

func isAlnum(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

// field reads up to the next unescaped delimiter and returns the text before it
// (backslashes kept, for the caller to interpret) and how much was consumed,
// including the delimiter.
func field(s string, delim rune) (string, int, bool) {
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\\' && i+w < len(s):
			_, w2 := utf8.DecodeRuneInString(s[i+w:])
			i += w + w2
		case r == delim:
			return s[:i], i + w, true
		default:
			i += w
		}
	}
	return "", 0, false
}

// ---- s ----

type subst struct {
	re     *regexp.Regexp
	repl   []replPart
	global bool
	nth    int // replace only the nth match (and later ones with g); 0 = first
}

type replPart struct {
	lit   string
	group int // -1: a literal; 0: the whole match (&); 1-9: \1..\9
}

func newSubst(pattern, repl, flags string, delim rune) (*subst, error) {
	sc := &subst{}
	ci := false
	for i := 0; i < len(flags); i++ {
		switch f := flags[i]; {
		case f == 'g':
			sc.global = true
		case f == 'i' || f == 'I':
			ci = true
		case f >= '1' && f <= '9':
			j := i
			for j < len(flags) && flags[j] >= '0' && flags[j] <= '9' {
				j++
			}
			sc.nth, _ = strconv.Atoi(flags[i:j])
			i = j - 1
		default:
			return nil, fmt.Errorf("s: unsupported flag %q (g, i, and a number are supported)", string(f))
		}
	}
	pat := unescapeDelim(pattern, delim)
	if ci {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("s: bad regular expression: %v", err)
	}
	sc.re = re
	sc.repl, err = parseRepl(repl, delim)
	if err != nil {
		return nil, err
	}
	for _, p := range sc.repl {
		if p.group > re.NumSubexp() {
			return nil, fmt.Errorf(`s: the replacement uses \%d, but the expression has %d group(s)`, p.group, re.NumSubexp())
		}
	}
	return sc, nil
}

// unescapeDelim turns \<delim> into <delim> in the expression. If the delimiter
// is itself special in a regular expression (s|a\|b|x|), the backslash stays and
// the character is literal.
func unescapeDelim(p string, delim rune) string {
	if strings.ContainsRune(`.[]()*+?^$|{}\`, delim) {
		return p
	}
	return strings.ReplaceAll(p, `\`+string(delim), string(delim))
}

// parseRepl reads the replacement: & is the whole match, \1..\9 the groups, \n a
// newline, \t a tab, \& a literal &, \<delim> the delimiter, \\ a backslash;
// any other \c is c.
func parseRepl(r string, delim rune) ([]replPart, error) {
	var parts []replPart
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, replPart{lit: lit.String(), group: -1})
			lit.Reset()
		}
	}
	for i := 0; i < len(r); {
		c, w := utf8.DecodeRuneInString(r[i:])
		i += w
		switch {
		case c == '&':
			flush()
			parts = append(parts, replPart{group: 0})
		case c == '\\' && i < len(r):
			e, w := utf8.DecodeRuneInString(r[i:])
			i += w
			switch {
			case e >= '1' && e <= '9':
				flush()
				parts = append(parts, replPart{group: int(e - '0')})
			case e == 'n':
				lit.WriteByte('\n')
			case e == 't':
				lit.WriteByte('\t')
			default:
				lit.WriteRune(e) // \& \\ \<delim> and any other
			}
		default:
			lit.WriteRune(c)
		}
	}
	flush()
	return parts, nil
}

func (c *subst) apply(in string) string {
	matches := c.re.FindAllStringSubmatchIndex(in, -1)
	if len(matches) == 0 {
		return in
	}
	var b strings.Builder
	last := 0
	for n, m := range matches {
		n++ // 1-based
		var hit bool
		switch {
		case c.global:
			hit = n >= max(c.nth, 1)
		case c.nth > 0:
			hit = n == c.nth
		default:
			hit = n == 1
		}
		if !hit {
			continue
		}
		b.WriteString(in[last:m[0]])
		for _, p := range c.repl {
			if p.group < 0 {
				b.WriteString(p.lit)
			} else if m[2*p.group] >= 0 {
				b.WriteString(in[m[2*p.group]:m[2*p.group+1]])
			}
		}
		last = m[1]
	}
	b.WriteString(in[last:])
	return b.String()
}

// ---- y ----

type transliterate struct{ to map[rune]rune }

func newTransliterate(from, to string, delim rune) (*transliterate, error) {
	f, err := yList(from, delim)
	if err != nil {
		return nil, err
	}
	t, err := yList(to, delim)
	if err != nil {
		return nil, err
	}
	if len(f) != len(t) {
		return nil, fmt.Errorf("y: the two lists must have the same length (%d and %d characters)", len(f), len(t))
	}
	m := make(map[rune]rune, len(f))
	for i, r := range f {
		if _, dup := m[r]; !dup {
			m[r] = t[i]
		}
	}
	return &transliterate{m}, nil
}

// yList reads one list of y: \\ is a backslash, \n a newline, \<delim> the
// delimiter; any other \c is c.
func yList(s string, delim rune) ([]rune, error) {
	var out []rune
	for i := 0; i < len(s); {
		c, w := utf8.DecodeRuneInString(s[i:])
		i += w
		if c == '\\' && i < len(s) {
			e, w := utf8.DecodeRuneInString(s[i:])
			i += w
			if e == 'n' {
				e = '\n'
			}
			c = e
		}
		out = append(out, c)
	}
	return out, nil
}

func (t *transliterate) apply(in string) string {
	return strings.Map(func(r rune) rune {
		if m, ok := t.to[r]; ok {
			return m
		}
		return r
	}, in)
}
