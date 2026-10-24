package spec

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file is the action side of the shared types: what the runner hands to an
// action, and the registry the action folders (action_script/, action_syslog/,
// ...) register into from init(), the same way functions do.
//
// exit, exitnow and rerun are not in this registry. They change how the program
// or the section runs, so the runner implements them itself.

// Failure is one failed ck.
type Failure struct {
	Ck      string `json:"ck"`      // the complete ck as written, tests included
	Message string `json:"message"` // why it failed (or its msg option)
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"` // where the ck is in the config (File is the file, for includes)
	// Options are all the options the function ran with, whether the config gave
	// them or not: the call's own, else [DEFAULTS], else the built-in value.
	// Absent for a check on a variable, which calls no function.
	Options *Options `json:"options,omitempty"`
}

// Options are the options in effect for one function call. Times are seconds.
type Options struct {
	Timeout float64 `json:"timeout"` // per attempt
	Retry   int     `json:"retry"`   // extra attempts after an error
	Sleep   float64 `json:"sleep"`   // pause between attempts
	Msg     string  `json:"msg,omitempty"`
}

// String writes the options like the config does: {timeout 2, retry 0, sleep 0}.
func (o Options) String() string {
	n := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	s := fmt.Sprintf("{timeout %s, retry %d, sleep %s", n(o.Timeout), o.Retry, n(o.Sleep))
	if o.Msg != "" {
		s += ", msg " + strconv.Quote(o.Msg)
	}
	return s + "}"
}

// Report is what an action is told about one section. It is also exactly what
// an as:IDENT script receives on stdin, as JSON.
type Report struct {
	Program  string    `json:"program"`
	Time     string    `json:"time"` // RFC 3339, UTC
	Section  string    `json:"section"`
	Failures []Failure `json:"failures"` // by failing ck, each with its message
}

// ActionRequest is what the runner passes to an action.
//
// Like functions, an action applies Timeout, Retry and Sleep itself, normally by
// running its blocking work through Attempt. The runner starts every action in
// its own goroutine.
type ActionRequest struct {
	Name   string // the registered action name, "as" for as:IDENT
	Arg    string // the IDENT of as:IDENT
	Script string // the body of as:IDENT, from [ACTION_SCRIPTS]
	Report Report

	// Options are this use's own options (action webhook {url "..."}), as text
	// with quotes removed. Settings are the [DEFAULTS] values for the keys the
	// action registered. An action decides which wins; normally the option.
	Options  map[string]string
	Settings map[string]string

	// Resolved like a function's: the use's own option, else [DEFAULTS], else
	// compiled-in.
	Timeout time.Duration // per attempt
	Retry   int           // extra attempts after an error
	Sleep   time.Duration // pause between attempts
}

// Opt returns one of this use's own options, or "".
func (r ActionRequest) Opt(key string) string { return r.Options[key] }

// Setting returns one of the action's [DEFAULTS] values, or "".
func (r ActionRequest) Setting(key string) string { return r.Settings[key] }

// Attempt runs op under the request's timeout, retry and sleep.
func (r ActionRequest) Attempt(ctx context.Context, op func(ctx context.Context) error) error {
	_, err := Attempt(ctx, Request{Timeout: r.Timeout, Retry: r.Retry, Sleep: r.Sleep}, func() (struct{}, error) {
		// A per-attempt context lets op stop what it started (kill a process,
		// abort an HTTP request) when the attempt times out, instead of leaving
		// it running in the background.
		actx := ctx
		if r.Timeout > 0 {
			var cancel context.CancelFunc
			actx, cancel = context.WithTimeout(ctx, r.Timeout)
			defer cancel()
		}
		return struct{}{}, op(actx)
	})
	return err
}

// ActionFunc does the real work. It returns nil when the action succeeded.
type ActionFunc func(ctx context.Context, req ActionRequest) error

// ActionSpec is everything the runner needs to know about one action.
type ActionSpec struct {
	Run ActionFunc

	// Options allowed after the action name, besides the standard ones (see
	// ActionOptions). Nil if there are none.
	Options map[string]Field

	// Settings are the [DEFAULTS] keys this action owns. Key names must be
	// unique across all actions, so prefix them (webhook_url, syslog_tag).
	Settings map[string]Field

	// Check, if set, runs while the config is validated, before anything runs,
	// so a missing url or path is reported up front. req has Options and
	// Settings filled in; Report is empty.
	Check func(req ActionRequest) error
}

// ActionOptions are the options every action that can hang accepts.
func ActionOptions() map[string]Field {
	return map[string]Field{
		"timeout": {Kind: KFloat}, // seconds (N.N), per attempt
		"retry":   {Kind: KInt},   // extra attempts after an error
		"sleep":   {Kind: KFloat}, // seconds (N.N) between attempts
	}
}

var (
	actions         = map[string]ActionSpec{}
	actionSettings  = map[string]Field{}
	reservedActions = map[string]bool{"exit": true, "exitnow": true, "rerun": true}
)

// RegisterAction adds an action. Call it from init(). It panics on a
// programming mistake, like Register does.
func RegisterAction(name string, as ActionSpec) {
	switch {
	case name == "":
		panic("spec.RegisterAction: empty action name")
	case strings.Contains(name, ":"):
		panic(fmt.Sprintf("spec.RegisterAction: action name %q must not contain ':'", name))
	case reservedActions[name]:
		panic(fmt.Sprintf("spec.RegisterAction: %q is built into the runner", name))
	case as.Run == nil:
		panic(fmt.Sprintf("spec.RegisterAction: action %q has no Run", name))
	}
	if _, dup := actions[name]; dup {
		panic(fmt.Sprintf("spec.RegisterAction: action %q is registered twice", name))
	}
	for key, f := range as.Settings {
		if _, dup := actionSettings[key]; dup {
			panic(fmt.Sprintf("spec.RegisterAction: [DEFAULTS] key %q is used by two actions", key))
		}
		actionSettings[key] = f
	}
	actions[name] = as
}

// LookupAction returns the action registered under name.
func LookupAction(name string) (ActionSpec, bool) {
	as, ok := actions[name]
	return as, ok
}

// ActionNames lists the registered actions in alphabetical order.
func ActionNames() []string {
	names := make([]string, 0, len(actions))
	for n := range actions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ActionSettingField returns the field for a [DEFAULTS] key owned by an action.
func ActionSettingField(key string) (Field, bool) {
	f, ok := actionSettings[key]
	return f, ok
}
