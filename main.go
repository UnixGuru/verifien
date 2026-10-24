package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"

	"verifien/spec"
)

// ---------------------------------------------------------------------------
// Lexer
// Rule order matters: Comment/Header/String are matched whole, so a '#' inside
// a header or string never starts a comment. DefaultsHeader and
// ActionScriptsHeader must precede Header. Keyword and ActionRef must precede
// Ident, and Operator must precede Punct so "==" is not split into two "=".
// ---------------------------------------------------------------------------

var dslLexer = lexer.MustSimple([]lexer.SimpleRule{
	{Name: "Comment", Pattern: `#[^\n]*`},
	{Name: "DefaultsHeader", Pattern: `(?i)\[\s*DEFAULTS\s*\]`},            // must precede Header
	{Name: "ActionScriptsHeader", Pattern: `(?i)\[\s*ACTION_SCRIPTS\s*\]`}, // must precede Header
	{Name: "Header", Pattern: `\[[^\]\n]*\]`},
	{Name: "String", Pattern: `"(\\.|[^"\\])*"|'(\\.|[^'\\])*'`},
	{Name: "Action", Pattern: `\baction\b`},
	{Name: "Include", Pattern: `\b(?:s?include)\b`}, // include "file-or-url", sinclude "file-or-url"
	{Name: "Keyword", Pattern: `\b(?:if|ck)\b`},
	{Name: "ActionRef", Pattern: `\bas:[a-zA-Z_][a-zA-Z0-9_]*`}, // as:IDENT, a script action
	{Name: "Regex", Pattern: `/(\\.|[^/\\\n])+/`},
	{Name: "Operator", Pattern: `==|!=|=~|!~|<=|>=|<|>`},
	{Name: "Number", Pattern: `[0-9]+(?:\.[0-9]+)?[A-Za-z]*`},
	{Name: "Ident", Pattern: `[a-zA-Z_][a-zA-Z0-9_]*|\$\?`}, // $? is read as exitstatus (see buildParser)
	{Name: "Punct", Pattern: `[(),={}.!]`},
	{Name: "Whitespace", Pattern: `\s+`},
})

// ---------------------------------------------------------------------------
// Grammar (pure syntax, identical for every function)
//
//   [DEFAULTS]                                              # optional global settings
//   timeout = 3
//   retry = 2
//
//   [section header]
//   variable = name("string", {options})                    # gather data; gates nothing
//   if name("string", {options}, test, test, ...)           # guard
//   ck name("string", {options}, test, test, ...)           # check
//   if variable.item OP value                               # guard on a variable
//   ck variable.item OP value                               # check on a variable
//   action exit, as:ident, syslog, rerun {retry 3}          # run if a ck in the section failed
//
//   [ACTION_SCRIPTS]                                        # optional
//   as:ident = "shell command"
// ---------------------------------------------------------------------------

type File struct {
	Items []*Item `@@*`
}

// Item is either a special block ([DEFAULTS] or [ACTION_SCRIPTS]) or an
// ordinary section.
type Item struct {
	Defaults *DefaultsSection `  @@`
	Scripts  *ScriptsSection  `| @@`
	Include  *Include         `| @@`
	Section  *Section         `| @@`
}

// DefaultsSection is [DEFAULTS]: global settings, written as one {options} block
// like the options of a call: [DEFAULTS] {timeout 10, retry 2, sleep = 0.5}
type DefaultsSection struct {
	Pos     lexer.Position
	Header  string   `@DefaultsHeader`
	Options *Options `@@`
}

// Sections returns the ordinary sections in file order (everything but [DEFAULTS]).
func (f *File) Sections() []*Section {
	var out []*Section
	for _, it := range f.Items {
		if it.Section != nil {
			out = append(out, it.Section)
		}
	}
	return out
}

type Section struct {
	Pos        lexer.Position
	Name       string       `@Header`
	Statements []*Statement `@@*`
}

// Title is the header text without the surrounding brackets.
func (s *Section) Title() string {
	return strings.TrimSpace(s.Name[1 : len(s.Name)-1])
}

// A statement is one of:
//
//	var = fn(...)            assignment (no keyword): runs fn and keeps the result
//	if|ck fn(...)            a function call with its own tests
//	if|ck var.item OP value  a test on an earlier variable
//
// An assignment starts with an identifier and the other two start with if/ck,
// so the first token tells them apart. After the keyword, "fn(" and "var."
// both start with an identifier, so the second token decides, which is why
// UseLookahead(2) is set below.
type Statement struct {
	Pos     lexer.Position
	Action  *Action `(  @@`
	Assign  *Assign `  | @@`
	Keyword string  `  | @Keyword`
	Not     bool    `    @"!"?` // if ! lstat(...): passes when the rest does not
	Call    *Call   `    ( @@`
	Check   *Check  `    | @@ ) )`
}

type Assign struct {
	Pos      lexer.Position
	Variable string `@Ident "="`
	Call     *Call  `@@`
}

type Check struct {
	Pos  lexer.Position
	Var  string `@Ident "."`
	Item string `@Ident`
	Op   string `@Operator`
	// The value is a literal, or an item of an earlier variable (ck b.perm == a.perm).
	RefVar  string `( @Ident "."`
	RefItem string `  @Ident`
	Value   *Value `| @@ )`
	// Only msg does anything: a check on a variable calls no function, so there
	// is nothing to time out or retry. Other options are ignored, not rejected.
	Options *Options `@@?`
}

type Call struct {
	Pos  lexer.Position
	Name string `@Ident "("`
	// The target is a string, or an item of an earlier variable (var.item), or
	// nothing at all: fn() has Target "".
	Target     string `( ( @String`
	TargetVar  string `    | @Ident "."`
	TargetItem string `      @Ident )`
	// More strings after the target, for the few functions that take them
	// (sed("text", "s/a/b/", "s/c/d/g")).
	Args    []string `  ("," @String)*`
	Options *Options `  ("," @@)?`
	Tests   []*Test  `  ("," @@)* )? ")"`
}

type Options struct {
	Items []*Option `"{" (@@ ("," @@)*)? "}"`
}

type Option struct {
	Pos   lexer.Position
	Key   string `@Ident "="?` // '=' is optional: "retry 3" and "retry = 3"
	Value *Value `@@`
}

type Test struct {
	Pos  lexer.Position
	Item string `@Ident`
	Op   string `@Operator`
	// The value is a literal, or an item of an earlier variable (perm == a.perm).
	RefVar  string `( @Ident "."`
	RefItem string `  @Ident`
	Value   *Value `| @@ )`
}

type Value struct {
	String *string `  @String`
	Number *string `| @Number`
	Ident  *string `| @Ident`
	Regex  *string `| @Regex`
}

// ---------------------------------------------------------------------------
// Functions
//
// The functions themselves live in folders by type (inode/, disk/, ...). Each
// one registers its spec from an init() (see package spec), and funcs.go lists
// which folders are linked in. Everything below only looks them up by name.
// ---------------------------------------------------------------------------

// funcSpec returns the spec of a function that is known to exist (Validate
// rejects unknown names before anything runs).
func funcSpec(name string) spec.FuncSpec {
	fs, _ := spec.Lookup(name)
	return fs
}

// ---------------------------------------------------------------------------
// Validation (before anything runs)
// ---------------------------------------------------------------------------

// Validate checks the whole file. Variables are per-section (sections run
// concurrently, so they cannot share state), are assigned once, and must be
// assigned before use; each remembers which function created it, so
// "var.size" is checked against that function's tests.
func (f *File) Validate(set Settings) error {
	for _, s := range f.Sections() {
		vars := map[string]*Assign{} // variable -> the assignment that created it
		cks := 0
		for _, st := range s.Statements {
			if st.Keyword == "ck" {
				cks++
			}
			switch {
			case st.Action != nil:
				// checked below, once the whole section has been seen

			case st.Assign != nil:
				a := st.Assign
				if err := checkTargetRef(a.Call, vars); err != nil {
					return err
				}
				if err := a.Call.Validate(vars); err != nil {
					return err
				}
				if len(a.Call.Tests) > 0 {
					return fmt.Errorf("%s: tests inside an assignment would gate nothing; put them on an 'if %s(...)' or 'ck %s(...)' line, or use 'ck %s.item OP value'",
						a.Call.Tests[0].Pos, a.Call.Name, a.Call.Name, a.Variable)
				}
				if prev, dup := vars[a.Variable]; dup {
					return fmt.Errorf("%s: variable %q is already assigned at %s", a.Pos, a.Variable, prev.Pos)
				}
				vars[a.Variable] = a

			case st.Call != nil:
				if err := checkTargetRef(st.Call, vars); err != nil {
					return err
				}
				if err := st.Call.Validate(vars); err != nil {
					return err
				}

			case st.Check != nil:
				c := st.Check
				a, ok := vars[c.Var]
				if !ok {
					return fmt.Errorf("%s: unknown variable %q (variables are per-section and must be assigned earlier)", c.Pos, c.Var)
				}
				fn := a.Call.Name
				if _, ok := funcSpec(fn).Tests[c.Item]; !ok {
					// labels (prometheus) are only tests inside the call itself
					return fmt.Errorf("%s: %s has no test %q", c.Pos, fn, c.Item)
				}
				if err := validateTest(fn, c.Pos, c.Item, c.Op, c.RefVar, c.RefItem, c.Value, vars); err != nil {
					return err
				}
				// Only msg means anything here. Any other option is ignored, not
				// rejected, but msg itself must be a string.
				if m, ok := optValue(c.Options, "msg"); ok {
					if err := checkValue(checkOptions["msg"], m); err != nil {
						return fmt.Errorf("%s: option msg: %w", c.Pos, err)
					}
				}
			}
		}
		if err := validateActions(s, sectionActions(s), cks, set); err != nil {
			return err
		}
	}
	return nil
}

// checkTargetRef checks a call whose target is var.item: the variable must have
// been assigned earlier in the section, and the function that made it must report
// that item.
func checkTargetRef(c *Call, vars map[string]*Assign) error {
	if c.TargetVar == "" {
		return nil
	}
	a, ok := vars[c.TargetVar]
	if !ok {
		return fmt.Errorf("%s: unknown variable %q in the target of %s (variables are per-section and must be assigned earlier)", c.Pos, c.TargetVar, c.Name)
	}
	f, ok := funcSpec(a.Call.Name).Tests[c.TargetItem]
	if !ok {
		return fmt.Errorf("%s: %s has no item %q (variable %s was made by %s)", c.Pos, a.Call.Name, c.TargetItem, c.TargetVar, a.Call.Name)
	}
	if f.Kind != spec.KString && !funcSpec(c.Name).TextInput {
		return fmt.Errorf("%s: %s.%s is not text, so it cannot be the target of %s (only text items can; sed takes any item)", c.Pos, c.TargetVar, c.TargetItem, c.Name)
	}
	return nil
}

// checkOptions are the options that mean something in `ck var.item OP value
// {options}`. Other options are accepted and ignored.
var checkOptions = map[string]spec.Field{"msg": {Kind: spec.KString}}

// unquoteString removes the quotes from a "..." or '...' token.
func unquoteString(raw string) string {
	in := raw[1 : len(raw)-1]
	var b strings.Builder
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c != '\\' || i+1 == len(in) {
			b.WriteByte(c)
			continue
		}
		i++
		switch in[i] {
		case '\\', '"', '\'':
			b.WriteByte(in[i])
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte('\\')
			b.WriteByte(in[i])
		}
	}
	return b.String()
}

func (c *Call) Validate(vars map[string]*Assign) error {
	fs, ok := spec.Lookup(c.Name)
	if !ok {
		return fmt.Errorf("%s: unknown function %q (known: %s)", c.Pos, c.Name, strings.Join(spec.Names(), ", "))
	}
	if fs.Args == nil && len(c.Args) > 0 {
		return fmt.Errorf("%s: %s takes only one string (the target) before its options and tests", c.Pos, c.Name)
	}
	if fs.Args != nil {
		if err := fs.Args(c.Args); err != nil {
			return fmt.Errorf("%s: %s: %w", c.Pos, c.Name, err)
		}
	}
	if c.Options != nil {
		for _, o := range c.Options.Items {
			f, ok := fs.Options[canonicalKey(o.Key)]
			if !ok {
				return fmt.Errorf("%s: %s has no option %q", o.Pos, c.Name, o.Key)
			}
			if err := checkValue(f, o.Value); err != nil {
				return fmt.Errorf("%s: option %s: %w", o.Pos, o.Key, err)
			}
			if canonicalKey(o.Key) == "timeout" {
				if f, _ := strconv.ParseFloat(*o.Value.Number, 64); seconds(f) <= 0 {
					return fmt.Errorf("%s: option timeout must be greater than 0", o.Pos)
				}
			}
		}
	}
	for _, t := range c.Tests {
		if err := validateTest(c.Name, t.Pos, t.Item, t.Op, t.RefVar, t.RefItem, t.Value, vars); err != nil {
			return err
		}
	}
	return nil
}

// validateTest checks one `item OP value` and adds the file position to any
// error. The checking itself is spec.ValidateTest, shared by every function.
func validateTest(fn string, pos lexer.Position, item, op, refVar, refItem string, v *Value, vars map[string]*Assign) error {
	if refVar != "" {
		a, ok := vars[refVar]
		if !ok {
			return fmt.Errorf("%s: unknown variable %q in the value of %s (variables are per-section and must be assigned earlier)", pos, refVar, item)
		}
		if err := spec.ValidateRef(fn, item, op, a.Call.Name, refItem); err != nil {
			return fmt.Errorf("%s: %s.%s: %w", pos, refVar, refItem, err)
		}
		return nil
	}
	if err := spec.ValidateTest(fn, item, op, spec.Value(*v)); err != nil {
		return fmt.Errorf("%s: %w", pos, err)
	}
	return nil
}

// checkValue checks an option or [DEFAULTS] value against its field.
func checkValue(f spec.Field, v *Value) error {
	return spec.CheckValue(f, spec.Value(*v))
}

// ---------------------------------------------------------------------------
// Running a call: timeout, retry
// ---------------------------------------------------------------------------

// Settings are the global values a call falls back on when it has no option of
// its own. Precedence: the call's own option > [DEFAULTS] > compiledDefaults.
type Settings struct {
	Timeout time.Duration // per attempt
	Retry   int           // extra attempts after an error
	Sleep   time.Duration // pause between attempts

	// Exit statuses, only settable in [DEFAULTS].
	ExitGood int // no exit/exitnow action fired
	ExitBad  int // action exit fired
	ExitNow  int // action exitnow fired

	// Defaults for "action rerun" (a section can override each with its own options).
	RerunRetry   int           // how many times to run the section again
	RerunSleep   time.Duration // pause before each rerun
	RerunTimeout time.Duration // limit for each rerun pass; 0 = none

	// Filled in by Settings() from the rest of the file, not from keys:
	Scripts    map[string]string // [ACTION_SCRIPTS]: IDENT -> shell command
	ActionVals map[string]string // [DEFAULTS] keys owned by actions (webhook_url, syslog_tag, ...)
}

// compiledDefaults apply when [DEFAULTS] is missing or does not set a key.
var compiledDefaults = Settings{Timeout: 10 * time.Second, Retry: 0, ExitGood: 0, ExitBad: 1, ExitNow: 1, RerunRetry: 1}

// What [DEFAULTS] may contain besides the keys that actions register for
// themselves. Values are plain numbers: timeout and rerun_timeout in seconds (N.N),
// retry and rerun_retry as a count, sleep and rerun_sleep in seconds (N.N),
// exit_status_* as a process exit status (0-255).
var settingFields = map[string]spec.Field{
	"timeout":          {Kind: spec.KFloat},
	"retry":            {Kind: spec.KInt},
	"sleep":            {Kind: spec.KFloat},
	"rerun_retry":      {Kind: spec.KInt},
	"rerun_sleep":      {Kind: spec.KFloat},
	"rerun_timeout":    {Kind: spec.KFloat},
	"exit_status_good": {Kind: spec.KInt},
	"exit_status_bad":  {Kind: spec.KInt},
	"exit_status_now":  {Kind: spec.KInt},
}

// Alternative spellings, accepted both in [DEFAULTS] and in call options.
var keyAlias = map[string]string{"retries": "retry"}

func canonicalKey(k string) string {
	if a, ok := keyAlias[k]; ok {
		return a
	}
	return k
}

// Settings merges [DEFAULTS] (if present) over the compiled-in defaults and
// validates it. By now expand has merged every file's [DEFAULTS] and
// [ACTION_SCRIPTS] into one block each.
func (f *File) Settings() (Settings, error) {
	set := compiledDefaults
	set.Scripts = map[string]string{}
	set.ActionVals = map[string]string{}

	given := map[string]bool{}
	for _, it := range f.Items {
		if sc := it.Scripts; sc != nil {
			for _, def := range sc.Scripts {
				ident := strings.TrimPrefix(def.Ref, "as:")
				if _, dup := set.Scripts[ident]; dup {
					return set, fmt.Errorf("%s: %s is defined twice", def.Pos, def.Ref)
				}
				if strings.TrimSpace(def.Body) == "" {
					return set, fmt.Errorf("%s: %s has an empty script", def.Pos, def.Ref)
				}
				set.Scripts[ident] = def.Body
			}
		}

		d := it.Defaults
		if d == nil {
			continue
		}
		for _, s := range d.Options.Items {
			key := canonicalKey(s.Key)
			fld, builtin := settingFields[key]
			if !builtin {
				var ok bool
				if fld, ok = spec.ActionSettingField(key); !ok {
					return set, fmt.Errorf("%s: unknown setting %q", s.Pos, s.Key)
				}
			}
			if given[key] {
				return set, fmt.Errorf("%s: setting %q is given twice", s.Pos, s.Key)
			}
			given[key] = true
			if err := checkValue(fld, s.Value); err != nil {
				return set, fmt.Errorf("%s: setting %s: %w", s.Pos, s.Key, err)
			}
			if !builtin {
				set.ActionVals[key] = spec.Value(*s.Value).Text() // owned by an action
				continue
			}

			// Built-in keys: every one is a number, so Value.Number is set.
			n, _ := strconv.Atoi(*s.Value.Number)
			secs, _ := strconv.ParseFloat(*s.Value.Number, 64)
			switch key {
			case "timeout":
				if seconds(secs) <= 0 {
					return set, fmt.Errorf("%s: setting timeout must be greater than 0", s.Pos)
				}
				set.Timeout = seconds(secs)
			case "retry":
				set.Retry = n
			case "sleep":
				set.Sleep = seconds(secs)
			case "rerun_retry":
				set.RerunRetry = n
			case "rerun_sleep":
				set.RerunSleep = seconds(secs)
			case "rerun_timeout":
				if seconds(secs) <= 0 {
					return set, fmt.Errorf("%s: setting rerun_timeout must be greater than 0", s.Pos)
				}
				set.RerunTimeout = seconds(secs)
			case "exit_status_good", "exit_status_bad", "exit_status_now":
				if n < 0 || n > 255 {
					return set, fmt.Errorf("%s: setting %s must be between 0 and 255", s.Pos, s.Key)
				}
				switch key {
				case "exit_status_good":
					set.ExitGood = n
				case "exit_status_bad":
					set.ExitBad = n
				case "exit_status_now":
					set.ExitNow = n
				}
			}
		}
	}
	return set, nil
}

// The call's own options. See optInt, optFloat and optValue in actions.go.

func intOption(c *Call, key string) (int, bool)       { return optInt(c.Options, key) }
func floatOption(c *Call, key string) (float64, bool) { return optFloat(c.Options, key) }

func stringOption(c *Call, key string) string {
	if v, ok := optValue(c.Options, key); ok && v.String != nil {
		return *v.String
	}
	return ""
}

// request builds what a function receives: the target string plus the timeout
// and retry that apply to this call (its own option if given, else [DEFAULTS],
// else the compiled-in value, all folded into set). Same for sleep. The runner does not enforce
// them; a function applies them to its blocking calls with spec.Attempt.
// effectiveOptions are the options a call runs with, for the report: all of them,
// including the ones the config did not mention.
func effectiveOptions(c *Call, set Settings) *spec.Options {
	req := request(c, set)
	return &spec.Options{
		Timeout: req.Timeout.Seconds(),
		Retry:   req.Retry,
		Sleep:   req.Sleep.Seconds(),
		Msg:     stringOption(c, "msg"),
	}
}

func request(c *Call, set Settings) spec.Request {
	req := spec.Request{Target: c.Target, Args: c.Args, Timeout: set.Timeout, Retry: set.Retry, Sleep: set.Sleep}
	if funcSpec(c.Name).NoDefaultRetry {
		req.Retry = 0
	}
	if f, ok := floatOption(c, "timeout"); ok {
		req.Timeout = seconds(f)
	}
	if n, ok := intOption(c, "retry"); ok {
		req.Retry = n
	}
	if f, ok := floatOption(c, "sleep"); ok {
		req.Sleep = seconds(f)
	}
	return req
}

// future is the eventual result of a call running in its own goroutine.
type future struct {
	done chan struct{}
	res  spec.Result
	err  error
}

// startCall runs the call in its own goroutine. If its target is an item of a
// variable (lstat(v.link)), dep is that variable: the call first waits for it, then
// uses the item's text as the target.
func startCall(ctx context.Context, c *Call, set Settings, r *sectionRun) *future {
	f := &future{done: make(chan struct{})}
	// The goroutine must not touch r.vars (the section adds to it as it goes), so
	// it gets a copy of just the variables this call refers to.
	deps := map[string]*varRun{}
	if c.TargetVar != "" {
		deps[c.TargetVar] = r.vars[c.TargetVar]
	}
	for _, t := range c.Tests {
		if t.RefVar != "" {
			deps[t.RefVar] = r.vars[t.RefVar]
		}
	}
	go func() {
		defer close(f.done)
		req := request(c, set)
		if c.TargetVar != "" {
			target, err := deps[c.TargetVar].item(ctx, c.TargetVar, c.TargetItem)
			if err != nil {
				f.err = err
				return
			}
			req.Target = target
		}
		fs := funcSpec(c.Name)
		if fs.Labels || fs.WantsTests {
			// the function applies its label tests itself (Labels), or wants to see
			// all the tests (WantsTests), so it needs their values
			for _, t := range c.Tests {
				if _, own := fs.Tests[t.Item]; own && !fs.WantsTests {
					continue
				}
				var want spec.Value
				if t.RefVar == "" {
					want = spec.Value(*t.Value)
				} else {
					var err error
					if want, err = wanted(ctx, deps, t.RefVar, t.RefItem, nil); err != nil {
						f.err = err
						return
					}
				}
				req.Tests = append(req.Tests, spec.TestRef{Item: t.Item, Op: t.Op, Want: want})
			}
		}
		f.res, f.err = fs.Run(ctx, req)
	}()
	return f
}

// needsVars reports whether a call can only start once variables exist: its
// target or one of its test values is an item of a variable.
func (c *Call) needsVars() bool {
	if c.TargetVar != "" {
		return true
	}
	for _, t := range c.Tests {
		if t.RefVar != "" {
			return true
		}
	}
	return false
}

// item returns the text of one item of the variable's result, for use as the
// target of another call.
func (v *varRun) item(ctx context.Context, name, item string) (string, error) {
	res, err := v.fut.wait(ctx)
	if err != nil {
		return "", fmt.Errorf("%s has no result: %v", name, err)
	}
	got, ok := res[item]
	if !ok {
		return "", fmt.Errorf("%s did not report %q", v.fn, item)
	}
	return spec.FormatItem(funcSpec(v.fn).Tests[item], got), nil
}

func (f *future) wait(ctx context.Context) (spec.Result, error) {
	select {
	case <-f.done:
		return f.res, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Evaluating tests
// ---------------------------------------------------------------------------

// evalTest checks one test with the shared spec.Evaluate.
func (r *sectionRun) evalTest(fn string, res spec.Result, item, op, refVar, refItem string, want *Value) (bool, string) {
	w, err := wanted(r.ctx, r.vars, refVar, refItem, want)
	if err != nil {
		return false, err.Error()
	}
	if refVar != "" {
		var err error
		if w, err = spec.PrepareWant(fn, item, op, w); err != nil {
			return false, err.Error()
		}
	}
	return spec.Evaluate(fn, res, item, op, w)
}

// wanted is the value a test compares with: the literal, or the item of an
// earlier variable (the test's validation has already made sure the types fit).
func wanted(ctx context.Context, vars map[string]*varRun, refVar, refItem string, lit *Value) (spec.Value, error) {
	if refVar == "" {
		return spec.Value(*lit), nil
	}
	v := vars[refVar]
	if v == nil {
		return spec.Value{}, fmt.Errorf("variable %s was never assigned", refVar)
	}
	res, err := v.fut.wait(ctx)
	if err != nil {
		return spec.Value{}, fmt.Errorf("%s has no result: %v", refVar, err)
	}
	got, ok := res[refItem]
	if !ok {
		return spec.Value{}, fmt.Errorf("%s did not report %q", v.fn, refItem)
	}
	return spec.ItemValue(funcSpec(v.fn).Tests[refItem], got), nil
}

// testText renders "item OP value" for the report.
func testText(item, op, refVar, refItem string, v *Value) string {
	if refVar != "" {
		return fmt.Sprintf("%s %s %s.%s", item, op, refVar, refItem)
	}
	return fmt.Sprintf("%s %s %s", item, op, spec.Value(*v).Literal())
}

// ---------------------------------------------------------------------------
// Running a section (see actions.go for runSection, the passes, rerun and the
// actions). exec below runs one statement.
// ---------------------------------------------------------------------------

type varRun struct {
	fn  string
	fut *future
}

type sectionRun struct {
	ctx  context.Context
	set  Settings
	vars map[string]*varRun // section-local variables
	futs map[*Call]*future  // calls already started (or finished)
}

// callOf returns the function call a statement makes, or nil for a check on a variable.
func (st *Statement) callOf() *Call {
	if st.Assign != nil {
		return st.Assign.Call
	}
	return st.Call
}

// prefetch starts every Async call in stmts right away, so slow ones overlap.
func (r *sectionRun) prefetch(stmts []*Statement) {
	for _, st := range stmts {
		// A call whose target is a variable's item starts when its statement
		// runs (futureFor), because the variable may not exist yet.
		if c := st.callOf(); c != nil && !c.needsVars() && funcSpec(c.Name).Async {
			r.futs[c] = startCall(r.ctx, c, r.set, r)
		}
	}
}

// futureFor returns the running call for c, starting it now if it was not
// started ahead of time.
func (r *sectionRun) futureFor(c *Call) *future {
	f := r.futs[c]
	if f == nil {
		f = startCall(r.ctx, c, r.set, r)
		r.futs[c] = f
	}
	return f
}

// callText renders a call for the report: name("target", test, test).
func callText(c *Call) string {
	s := c.Name + "("
	sep := ""
	switch {
	case c.TargetVar != "":
		s += c.TargetVar + "." + c.TargetItem
		sep = ", "
	case c.Target != "" || len(c.Tests) > 0 || len(c.Args) > 0:
		s += fmt.Sprintf("%q", c.Target)
		sep = ", "
	}
	for _, a := range c.Args {
		s += sep + fmt.Sprintf("%q", a)
		sep = ", "
	}
	for _, t := range c.Tests {
		s += sep + testText(t.Item, t.Op, t.RefVar, t.RefItem, t.Value)
		sep = ", "
	}
	return s + ")"
}

// failMsg is the message shown when a statement fails. Every function has its
// own fixed message (detail); the call's msg option, if the user gave one,
// replaces it.
func failMsg(c *Call, detail string) string {
	if m := stringOption(c, "msg"); m != "" {
		return m
	}
	return detail
}

// exec runs one statement. A leading ! (if ! lstat(...), ck ! v.size > 3) turns
// the answer around: the statement passes when the call fails, errors, or any of
// its tests is false, and fails when the call and all its tests hold.
func (r *sectionRun) exec(st *Statement) StmtResult {
	out := r.execInner(st)
	if !st.Not {
		return out
	}
	out.Text = "! " + out.Text
	if !out.Pass {
		out.Pass, out.Detail = true, ""
		return out
	}
	out.Pass = false
	msg := ""
	switch {
	case st.Call != nil:
		msg = stringOption(st.Call, "msg")
	case st.Check != nil:
		if m, ok := optValue(st.Check.Options, "msg"); ok && m.String != nil {
			msg = *m.String
		}
	}
	if msg == "" {
		msg = "the statement is true, but the ! says it must not be"
	}
	out.Detail = msg
	return out
}

func (r *sectionRun) execInner(st *Statement) StmtResult {
	out := StmtResult{Pos: st.Pos, Keyword: st.Keyword}
	switch {
	case st.Assign != nil:
		// Gathers data and gates nothing. It only fails if the call itself fails.
		a := st.Assign
		out.Text = fmt.Sprintf("%s = %s", a.Variable, callText(a.Call))
		out.Options = effectiveOptions(a.Call, r.set)
		fut := r.futureFor(a.Call)
		r.vars[a.Variable] = &varRun{a.Call.Name, fut}
		if _, err := fut.wait(r.ctx); err != nil {
			out.Detail = failMsg(a.Call, err.Error())
			return out
		}
		out.Pass = true

	case st.Call != nil:
		// if|ck fn(...): passes when the call succeeds and all its tests are true.
		c := st.Call
		out.Text = callText(c)
		out.Options = effectiveOptions(c, r.set)
		res, err := r.futureFor(c).wait(r.ctx)
		if err != nil {
			out.Detail = failMsg(c, err.Error())
			return out
		}
		for _, t := range c.Tests {
			if _, own := funcSpec(c.Name).Tests[t.Item]; !own && funcSpec(c.Name).Labels {
				continue // a label test: the function already applied it
			}
			if ok, detail := r.evalTest(c.Name, res, t.Item, t.Op, t.RefVar, t.RefItem, t.Value); !ok {
				out.Detail = failMsg(c, detail)
				return out
			}
		}
		out.Pass = true

	case st.Check != nil:
		c := st.Check
		out.Text = c.Var + "." + testText(c.Item, c.Op, c.RefVar, c.RefItem, c.Value)
		v := r.vars[c.Var]
		if v == nil {
			out.Detail = fmt.Sprintf("variable %s was never assigned", c.Var)
			return out
		}
		// msg replaces whatever message the failure would have had.
		msg := func(detail string) string {
			if m, ok := optValue(c.Options, "msg"); ok && m.String != nil {
				return *m.String
			}
			return detail
		}
		res, err := v.fut.wait(r.ctx)
		if err != nil {
			out.Detail = msg(fmt.Sprintf("%s has no result: %v", c.Var, err))
			return out
		}
		if ok, detail := r.evalTest(v.fn, res, c.Item, c.Op, c.RefVar, c.RefItem, c.Value); !ok {
			out.Detail = msg(detail)
			return out
		}
		out.Pass = true
	}
	return out
}

// ---------------------------------------------------------------------------
// Parse, run, report
// ---------------------------------------------------------------------------

func buildParser() *participle.Parser[File] {
	return participle.MustBuild[File](
		participle.Lexer(dslLexer),
		participle.Elide("Comment", "Whitespace"),
		// Unquote strings, forgiving about backslashes: \\ \" \' \n \t \r mean what they
		// say; any other \x is kept as written, so "s/^/\//" and "\d+" need no doubling.
		participle.Map(func(t lexer.Token) (lexer.Token, error) {
			t.Value = unquoteString(t.Value)
			return t, nil
		}, "String"),
		// $? is the shell's name for the exit status: it is an alias of the item
		// exitstatus (which cmd has), and is the item everywhere it is written.
		participle.Map(func(t lexer.Token) (lexer.Token, error) {
			if t.Value == "$?" {
				t.Value = "exitstatus"
			}
			return t, nil
		}, "Ident"),
		// Strip the slashes from /regex/ literals; the pattern itself is untouched.
		participle.Map(func(t lexer.Token) (lexer.Token, error) {
			t.Value = t.Value[1 : len(t.Value)-1]
			return t, nil
		}, "Regex"),
		// 2 tokens: after ",", tell "{" (options) from an identifier (test);
		// after if/ck, tell "fn(" (a call) from "var." (a check on a variable).
		participle.UseLookahead(2),
	)
}

func parseAndValidate(parser *participle.Parser[File], name, src string) (*File, Settings, error) {
	f, err := parser.ParseString(name, src)
	if err != nil {
		return nil, Settings{}, err
	}
	f, err = expand(parser, f, name, []string{identity(name)})
	if err != nil {
		return nil, Settings{}, err
	}
	set, err := f.Settings()
	if err != nil {
		return nil, Settings{}, err
	}
	if err := f.Validate(set); err != nil {
		return nil, Settings{}, err
	}
	return f, set, nil
}

// report prints results in file order and returns the exit status:
//
//	exitnow ran                  -> set.ExitNow (default 1)
//	exit ran, or Ctrl-C aborted  -> set.ExitBad (default 1)
//	otherwise                    -> set.ExitGood (default 0)
//
// A failed ck on its own is reported but does not change the status; only an
// action does. If both actions ran, exitnow wins.
//
// The text report (sections, PASS/FAIL, ACTION lines) is printed only when debug
// is true; the exit status is the same either way.
func report(results []SectionResult, set Settings, debug bool) int {
	if !debug {
		return render(io.Discard, results, set)
	}
	return render(os.Stdout, results, set)
}

// render is report writing to w, so tests can read what was printed.
func render(w io.Writer, results []SectionResult, set Settings) int {
	var exit, exitNow bool
	for _, sr := range results {
		fmt.Fprintf(w, "[%s]\n", sr.Name)
		for _, st := range sr.Stmts {
			status := "PASS"
			if !st.Pass {
				status = "FAIL"
			}
			kw := st.Keyword
			if kw == "" {
				kw = "-"
			}
			line := fmt.Sprintf("  %s  %-2s  %s  %s", status, kw, fmt.Sprintf("%s:%d", st.Pos.Filename, st.Pos.Line), st.Text)
			if st.Options != nil {
				line += "  " + st.Options.String()
			}
			if st.Detail != "" {
				line += "  -> " + st.Detail
			}
			fmt.Fprintln(w, line)
		}
		if sr.Skipped {
			fmt.Fprintln(w, "  SKIP  ck statements not run (an if did not pass)")
		}
		for _, n := range sr.Notes {
			fmt.Fprintf(w, "  %s\n", n)
		}
		if sr.Reruns > 0 {
			if sr.Failed {
				fmt.Fprintf(w, "  RERUN  still failing after %d rerun(s) (max %d)\n", sr.Reruns, sr.RerunMax)
			} else {
				fmt.Fprintf(w, "  RERUN  passed on rerun %d of %d\n", sr.Reruns, sr.RerunMax)
			}
		}
		if sr.Aborted {
			fmt.Fprintln(w, "  ABORT  not finished: stopped by exitnow in another section (or Ctrl-C)")
			exit = true
		}
		for _, a := range sr.Actions {
			if a.Err != "" {
				fmt.Fprintf(w, "  ACTION  %s  FAILED: %s\n", a.Name, a.Err)
			} else {
				fmt.Fprintf(w, "  ACTION  %s\n", a.Name)
			}
			switch a.Name {
			case "exitnow":
				exit, exitNow = true, true
			case "exit":
				exit = true
			}
		}
	}
	switch {
	case exitNow:
		return set.ExitNow
	case exit:
		return set.ExitBad
	}
	return set.ExitGood
}

const defaultConfig = "verifien.cfg" // read from the current working directory

// options are the command line options.
type options struct {
	cfg   string // path or http(s) URL of the config; "" means ./verifien.cfg
	debug bool   // print the report (sections, PASS/FAIL, ACTION lines)
}

func parseArgs(args []string, usageOut io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("verifien", flag.ContinueOnError)
	fs.SetOutput(usageOut)
	fs.StringVar(&o.cfg, "cfg", "", "path or http(s) URL of the config file, used instead of ./"+defaultConfig)
	fs.BoolVar(&o.debug, "debug", false, "print the report: sections, PASS/FAIL and ACTION lines")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q (see -help)", fs.Arg(0))
	}
	return o, nil
}

// loadConfig reads the config named by --cfg (a file or a URL), or ./verifien.cfg
// when there is none, then expands its includes, and validates it. With --cfg the
// default file is not read at all.
func loadConfig(parser *participle.Parser[File], cfg string) (*File, Settings, error) {
	if cfg == "" {
		cfg = defaultConfig
	}
	b, _, err := readSource(cfg)
	if err != nil {
		return nil, Settings{}, err
	}
	return parseAndValidate(parser, cfg, string(b))
}

func main() {
	opts, err := parseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, stopAll := context.WithCancel(sigCtx) // cancelled by Ctrl-C or by exitnow
	defer stopAll()

	f, set, err := loadConfig(buildParser(), opts.cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	results := runAll(ctx, f, set, stopAll)
	code := report(results, set, opts.debug)
	stopAll()
	stop()
	os.Exit(code)
}

// runAll runs every section, each in its own goroutine, and returns the results
// in file order. stopAll cancels all of them (exitnow uses it).
func runAll(ctx context.Context, f *File, set Settings, stopAll context.CancelFunc) []SectionResult {
	sections := f.Sections()
	results := make([]SectionResult, len(sections))
	var wg sync.WaitGroup
	for i, s := range sections {
		wg.Add(1)
		go func(i int, s *Section) { // each goroutine writes only its own slot
			defer wg.Done()
			results[i] = runSection(ctx, s, set, stopAll)
		}(i, s)
	}
	wg.Wait()
	return results
}
