// Package spec holds the types shared by the runner (package main) and the
// function implementations that live in their own folders (inode/, disk/, ...),
// plus the code that every function shares:
//
//   - spec.go: the types passed to and returned from a function, and the
//     registry of functions.
//   - attempt.go: Attempt, which runs a blocking call in a goroutine under the
//     timeout and retry settings.
//   - check.go: the one implementation of "item OP value" testing.
//
// A function folder registers each of its functions from an init() func:
//
//	func init() {
//		spec.Register("stat", spec.FuncSpec{Run: stat, ...})
//	}
//
// and main imports the folder with a blank import (see funcs.go) so those
// init() funcs run before main starts.
package spec

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Kind says what type of value an option or a test item holds.
type Kind int

const (
	KString Kind = iota
	KInt
	KSize // 3, 3K, 3M, 3G, 3T (powers of 1024)
	KEnum
	KFloat // 0.5, 2, 1.25: for options such as sleep (not usable in tests)
	KUID   // a user: id (int64) tested against a number or a login name
	KGID   // a group: id (int64) tested against a number or a group name
	KPerm  // permission bits (int64), tested like find -perm: see perm.go
)

// Field describes one allowed option or test item.
type Field struct {
	Kind Kind
	Enum []string // for KEnum
	Ops  []string // allowed operators for tests; nil = any
	// Any (enum items only) is a value that stands for "whatever the item reports":
	// == Any is true when the function reported a value at all, != Any when it
	// reported "" (nothing). stat's type == e means "the inode exists".
	Any string
	// Loose (text items only): the value may also be written as a number or a bare
	// word, and is compared as the text it was written as (package == 0).
	Loose bool
}

// Result is what a function reports: test item name -> value.
// KInt, KSize, KUID, KGID and KPerm items must be int64; KString and KEnum items
// must be string.
type Result map[string]any

// Request is what the runner passes to a function.
//
// The runner does not enforce Timeout or Retry itself. A function applies them
// to its blocking calls, normally by running each one through Attempt. A
// function that never blocks does not need to.
type Request struct {
	Target  string        // the quoted string from the config file
	Timeout time.Duration // per attempt: the call's own option, else [DEFAULTS], else compiled-in
	Retry   int           // extra attempts after an error, so at most Retry+1 attempts in all
	Sleep   time.Duration // pause between attempts (not before the first or after the last)
	// Tests are the call's label tests, for a function with FuncSpec.Labels: the
	// function applies them itself. For every other function this is empty.
	Tests []TestRef
	// Args are the extra strings after the target (FuncSpec.Args), in order.
	Args []string
}

// TestRef is one `item OP value` test handed to a function that applies its own
// tests. Want is already resolved: a variable's item has become its value.
type TestRef struct {
	Item string
	Op   string
	Want Value
}

// Func does the real work. It should return promptly when ctx is cancelled.
type Func func(ctx context.Context, req Request) (Result, error)

// StdOptions returns the options every function accepts. A function normally
// uses it as its Options table; add a new standard option here and every
// function gets it.
func StdOptions() map[string]Field {
	return map[string]Field{
		"timeout": {Kind: KFloat}, // seconds (N.N), per attempt
		"retry":   {Kind: KInt},   // extra attempts after an error
		"sleep":   {Kind: KFloat}, // seconds (N.N) between attempts
		"msg":     {Kind: KString},
	}
}

// FuncSpec is everything the runner needs to know about one function.
type FuncSpec struct {
	Run     Func
	Async   bool             // start as soon as its section starts, so it can overlap with others
	Options map[string]Field // what may appear inside {}
	Tests   map[string]Field // what may appear as the item in `item OP value`
	// Labels: any other item name is allowed in a test and means a label. A label
	// is a text item (Loose, operators == != =~ !~). The runner does not evaluate
	// label tests; the function gets them in Request.Tests and applies them,
	// because they must all hold for the same series.
	Labels bool
	// Args: the function takes extra strings after the target (sed("text", "s/a/b/")).
	// It checks them before anything runs (and is told when there are none). nil
	// means the function takes none, and giving one is a config error.
	Args func(args []string) error
	// TextInput: the target is plain text, so any item of a variable (a number, a
	// permission, ...) may be used as the target, by its text: 0640, 1000, 4096.
	TextInput bool
	// NoDefaultRetry: [DEFAULTS] retry does not apply; the function retries only
	// when the call itself says so ({retry 2}). For functions that do things.
	NoDefaultRetry bool
	// WantsTests: the function gets all the call's tests in Request.Tests (not only
	// the label tests of a Labels function), to decide what to report. The runner
	// still evaluates the tests itself afterwards.
	WantsTests bool
}

// LabelField is the field of a label item of a function with Labels set.
var LabelField = Field{Kind: KString, Loose: true, Ops: []string{"==", "!=", "=~", "!~"}}

// TestField returns the field of a test item: one of the function's own Tests,
// or, for a function with Labels, a label.
func (fs FuncSpec) TestField(item string) (Field, bool) {
	if f, ok := fs.Tests[item]; ok {
		return f, true
	}
	if fs.Labels {
		return LabelField, true
	}
	return Field{}, false
}

var registry = map[string]FuncSpec{}

// Register adds a function. It is meant to be called from init() funcs, which
// run one at a time before main, so it needs no locking. It panics on a
// programming mistake (empty name, nil Run, duplicate name) so that a bad
// function fails at startup instead of in the middle of a run.
func Register(name string, fs FuncSpec) {
	if name == "" {
		panic("spec.Register: empty function name")
	}
	if fs.Run == nil {
		panic(fmt.Sprintf("spec.Register: function %q has no Run", name))
	}
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("spec.Register: function %q is registered twice", name))
	}
	registry[name] = fs
}

// Lookup returns the function registered under name.
func Lookup(name string) (FuncSpec, bool) {
	fs, ok := registry[name]
	return fs, ok
}

// Names lists the registered functions in alphabetical order.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
