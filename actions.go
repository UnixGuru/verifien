package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/participle/v2/lexer"

	"verifien/spec"
)

// ---------------------------------------------------------------------------
// Actions: grammar
//
//   action exit
//   action as:cleanup, syslog, exit
//   action webhook {url "https://hooks.example.com/x", timeout 5, retry 2}
//   action rerun {retry 3, sleep 2, timeout 30}, opentel
//
// An action line is a comma list. Each item is a name with optional {options}.
// A section may have several action lines; they are added together.
//
// Scripts for as:IDENT are defined in their own block:
//
//   [ACTION_SCRIPTS]
//   as:cleanup = "logger -t verifien failed"
// ---------------------------------------------------------------------------

type Action struct {
	Pos   lexer.Position
	Kw    string        `@Action`
	Items []*ActionItem `@@ ("," @@)*`
}

type ActionItem struct {
	Pos     lexer.Position
	Name    string   `@(ActionRef | Ident)`
	Options *Options `@@?`
}

// ScriptsSection is [ACTION_SCRIPTS]: one `as:IDENT = "shell command"` per line.
type ScriptsSection struct {
	Pos     lexer.Position
	Header  string       `@ActionScriptsHeader`
	Scripts []*ScriptDef `@@*`
}

type ScriptDef struct {
	Pos  lexer.Position
	Ref  string `@ActionRef "="`
	Body string `@String`
}

// splitAction splits "as:cleanup" into ("as", "cleanup"). A name without a
// colon comes back as (name, "").
func splitAction(name string) (base, arg string) {
	if i := strings.IndexByte(name, ':'); i >= 0 {
		return name[:i], name[i+1:]
	}
	return name, ""
}

// builtinAction reports whether the runner itself implements the action.
func builtinAction(name string) bool {
	return name == "exit" || name == "exitnow" || name == "rerun"
}

// sectionActions returns every action item of a section, in the order written.
func sectionActions(s *Section) []*ActionItem {
	var items []*ActionItem
	for _, st := range s.Statements {
		if st.Action != nil {
			items = append(items, st.Action.Items...)
		}
	}
	return items
}

// ---------------------------------------------------------------------------
// Options on an action item
// ---------------------------------------------------------------------------

func optValue(o *Options, key string) (*Value, bool) {
	if o != nil {
		for _, it := range o.Items {
			if canonicalKey(it.Key) == key {
				return it.Value, true
			}
		}
	}
	return nil, false
}

func optInt(o *Options, key string) (int, bool) {
	if v, ok := optValue(o, key); ok && v.Number != nil {
		if n, err := strconv.Atoi(*v.Number); err == nil {
			return n, true
		}
	}
	return 0, false
}

func optFloat(o *Options, key string) (float64, bool) {
	if v, ok := optValue(o, key); ok && v.Number != nil {
		if f, err := strconv.ParseFloat(*v.Number, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

// optionTexts returns an item's options as text, keys in canonical form.
func optionTexts(o *Options) map[string]string {
	m := map[string]string{}
	if o != nil {
		for _, it := range o.Items {
			m[canonicalKey(it.Key)] = spec.Value(*it.Value).Text()
		}
	}
	return m
}

// validateOptions checks an item's options against the allowed fields.
func validateOptions(o *Options, fields map[string]spec.Field, what string) error {
	if o == nil {
		return nil
	}
	for _, it := range o.Items {
		key := canonicalKey(it.Key)
		f, ok := fields[key]
		if !ok {
			return fmt.Errorf("%s: %s has no option %q", it.Pos, what, it.Key)
		}
		if err := checkValue(f, it.Value); err != nil {
			return fmt.Errorf("%s: option %s: %w", it.Pos, it.Key, err)
		}
		if key == "timeout" {
			if f, _ := strconv.ParseFloat(*it.Value.Number, 64); seconds(f) <= 0 {
				return fmt.Errorf("%s: option timeout must be greater than 0", it.Pos)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// actionFields are the options an action item may carry: the standard three
// (timeout, retry, sleep) plus whatever its own spec adds.
func actionFields(as spec.ActionSpec) map[string]spec.Field {
	fields := spec.ActionOptions()
	for k, f := range as.Options {
		fields[k] = f
	}
	return fields
}

func knownActionList() string {
	known := []string{"exit", "exitnow", "rerun", "as:IDENT"}
	for _, n := range spec.ActionNames() {
		if n != "as" {
			known = append(known, n)
		}
	}
	return strings.Join(known, ", ")
}

// validateActions checks the action items of one section. cks is the number of
// ck statements in the section.
func validateActions(s *Section, items []*ActionItem, cks int, set Settings) error {
	seen := map[string]*ActionItem{}
	for _, it := range items {
		if prev, dup := seen[it.Name]; dup {
			return fmt.Errorf("%s: action %s is already given at %s", it.Pos, it.Name, prev.Pos)
		}
		seen[it.Name] = it

		switch it.Name {
		case "exit", "exitnow":
			if it.Options != nil {
				return fmt.Errorf("%s: action %s takes no options", it.Pos, it.Name)
			}
			continue
		case "rerun":
			if err := validateOptions(it.Options, spec.ActionOptions(), "rerun"); err != nil {
				return err
			}
			continue
		}

		base, arg := splitAction(it.Name)
		as, ok := spec.LookupAction(base)
		if !ok {
			return fmt.Errorf("%s: unknown action %q (known: %s)", it.Pos, it.Name, knownActionList())
		}
		switch {
		case base == "as" && arg == "":
			return fmt.Errorf("%s: write as:IDENT, where IDENT is defined in [ACTION_SCRIPTS]", it.Pos)
		case base != "as" && arg != "":
			return fmt.Errorf("%s: action %s does not take :%s", it.Pos, base, arg)
		case base == "as":
			if _, ok := set.Scripts[arg]; !ok {
				return fmt.Errorf("%s: as:%s is not defined in [ACTION_SCRIPTS]", it.Pos, arg)
			}
		}
		if err := validateOptions(it.Options, actionFields(as), it.Name); err != nil {
			return err
		}
		if as.Check != nil {
			if err := as.Check(buildRequest(it, set, spec.Report{})); err != nil {
				return fmt.Errorf("%s: action %s: %w", it.Pos, it.Name, err)
			}
		}
	}
	if len(items) > 0 && cks == 0 {
		return fmt.Errorf("%s: action %s in section [%s] can never run: the section has no ck", items[0].Pos, items[0].Name, s.Title())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Running actions
// ---------------------------------------------------------------------------

// ActionOutcome is what happened to one action of a section.
type ActionOutcome struct {
	Name string
	Err  string // empty when it succeeded
}

// buildRequest resolves everything an action needs: its own options, the
// [DEFAULTS] values, and timeout/retry/sleep (the item's own option, else
// [DEFAULTS], else compiled-in).
func buildRequest(it *ActionItem, set Settings, rep spec.Report) spec.ActionRequest {
	base, arg := splitAction(it.Name)
	req := spec.ActionRequest{
		Name:     base,
		Arg:      arg,
		Report:   rep,
		Options:  optionTexts(it.Options),
		Settings: set.ActionVals,
		Timeout:  set.Timeout,
		Retry:    set.Retry,
		Sleep:    set.Sleep,
	}
	if arg != "" {
		req.Script = set.Scripts[arg]
	}
	if f, ok := optFloat(it.Options, "timeout"); ok {
		req.Timeout = seconds(f)
	}
	if n, ok := optInt(it.Options, "retry"); ok {
		req.Retry = n
	}
	if f, ok := optFloat(it.Options, "sleep"); ok {
		req.Sleep = seconds(f)
	}
	return req
}

func newReport(section string, failures []spec.Failure) spec.Report {
	return spec.Report{
		Program:  "verifien",
		Time:     time.Now().UTC().Format(time.RFC3339),
		Section:  section,
		Failures: failures,
	}
}

// runEffects runs the section's side-effect actions (everything but exit,
// exitnow and rerun), each in its own goroutine, and waits for all of them.
// Each action applies its own timeout, retry and sleep, so none can hold up the
// others for longer than its own limits. Outcomes come back in written order.
func runEffects(ctx context.Context, items []*ActionItem, rep spec.Report, set Settings) []ActionOutcome {
	outs := make([]ActionOutcome, 0, len(items)) // never grows, so &outs[i] stays valid
	var wg sync.WaitGroup
	for _, it := range items {
		if builtinAction(it.Name) {
			continue
		}
		base, _ := splitAction(it.Name)
		as, _ := spec.LookupAction(base)
		req := buildRequest(it, set, rep)

		outs = append(outs, ActionOutcome{Name: it.Name})
		out := &outs[len(outs)-1]
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					out.Err = fmt.Sprintf("panic: %v", r)
				}
			}()
			if err := as.Run(ctx, req); err != nil {
				out.Err = err.Error()
			}
		}()
	}
	wg.Wait()
	return outs
}

// ---------------------------------------------------------------------------
// rerun
// ---------------------------------------------------------------------------

type rerunCfg struct {
	Retry   int           // how many times to run the section again
	Sleep   time.Duration // pause before each rerun
	Timeout time.Duration // limit for each rerun pass; 0 = none
}

// rerunFor returns the section's rerun settings: its own options, else
// [DEFAULTS] (rerun_retry, rerun_sleep, rerun_timeout), else compiled-in.
func rerunFor(items []*ActionItem, set Settings) (rerunCfg, bool) {
	for _, it := range items {
		if it.Name != "rerun" {
			continue
		}
		c := rerunCfg{Retry: set.RerunRetry, Sleep: set.RerunSleep, Timeout: set.RerunTimeout}
		if n, ok := optInt(it.Options, "retry"); ok {
			c.Retry = n
		}
		if f, ok := optFloat(it.Options, "sleep"); ok {
			c.Sleep = seconds(f)
		}
		if f, ok := optFloat(it.Options, "timeout"); ok {
			c.Timeout = seconds(f)
		}
		return c, true
	}
	return rerunCfg{}, false
}

// sleepCtx sleeps for d; it returns false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// ---------------------------------------------------------------------------
// Running a section
//
// A pass runs the section's statements once:
//   Phase 1: plain assignments and every "if", in order. The first "if" that
//            fails stops the pass: its "ck" statements are skipped.
//   Phase 2: every "ck", in order, only if all "if" passed.
//
// Functions marked Async are started up front for each phase, so several slow
// calls run at the same time; a statement only waits when it needs the result.
//
// runSection runs one pass. If a ck failed and the section has "action rerun",
// it sleeps and runs further passes (up to the retry count) until one has no
// failed ck. Then, if the last pass still has failed cks, the side-effect
// actions run, all at once, each in its own goroutine, and only then exit or
// exitnow takes effect.
// ---------------------------------------------------------------------------

type StmtResult struct {
	Pos     lexer.Position
	Options *spec.Options // what the function ran with; nil for a check on a variable
	Keyword string
	Text    string
	Pass    bool
	Detail  string
}

type SectionResult struct {
	Name     string
	Skipped  bool // an "if" failed, so the "ck" statements were not run
	Aborted  bool // cancelled before finishing (another section ran exitnow, or Ctrl-C)
	Failed   bool // the last pass had at least one failed ck
	Reruns   int  // how many extra passes rerun made
	RerunMax int  // how many it was allowed to make
	Stmts    []StmtResult
	Notes    []string
	Actions  []ActionOutcome // actions that ran because a ck failed
}

// passResult is one run of a section's statements.
type passResult struct {
	stmts    []StmtResult
	failures []spec.Failure // failed cks, in order
	skipped  bool
	aborted  bool
	timedOut bool
}

// runPass runs the section's statements once. timeout limits the pass (0 =
// none). With stopAtFirstFail the pass ends at the first failed ck, which is how
// exitnow stops "now".
func runPass(parent context.Context, s *Section, set Settings, timeout time.Duration, stopAtFirstFail bool) passResult {
	ctx, cancel := context.WithCancel(parent) // cancels leftover background calls on return
	if timeout > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(parent, timeout)
	}
	defer cancel()

	r := &sectionRun{
		ctx:  ctx,
		set:  set,
		vars: map[string]*varRun{},
		futs: map[*Call]*future{},
	}
	var pr passResult

	// timedOut notes when this pass ran out of time (not when the whole program
	// was cancelled).
	timedOut := func() bool {
		if ctx.Err() != nil && parent.Err() == nil {
			pr.timedOut = true
			pr.failures = append(pr.failures, spec.Failure{
				Ck:      "(section)",
				Message: fmt.Sprintf("section pass timed out after %s", timeout),
			})
			return true
		}
		return false
	}

	var phase1, phase2 []*Statement
	for _, st := range s.Statements {
		switch {
		case st.Action != nil:
		case st.Keyword == "ck":
			phase2 = append(phase2, st)
		default:
			phase1 = append(phase1, st)
		}
	}

	r.prefetch(phase1)
	for _, st := range phase1 {
		res := r.exec(st)
		if parent.Err() != nil { // cancelled while running: the result is not meaningful
			pr.aborted = true
			return pr
		}
		pr.stmts = append(pr.stmts, res)
		if timedOut() {
			return pr
		}
		if st.Keyword == "if" && !res.Pass {
			pr.skipped = true
			return pr
		}
	}

	r.prefetch(phase2)
	for _, st := range phase2 {
		res := r.exec(st)
		if parent.Err() != nil {
			pr.aborted = true
			return pr
		}
		pr.stmts = append(pr.stmts, res)
		if !res.Pass {
			pr.failures = append(pr.failures, spec.Failure{
				Ck: res.Text, Message: res.Detail,
				File: res.Pos.Filename, Line: res.Pos.Line, Options: res.Options,
			})
		}
		if timedOut() {
			return pr
		}
		if !res.Pass && stopAtFirstFail {
			return pr
		}
	}
	return pr
}

// runSection runs one section. stopAll cancels every section; exitnow uses it.
func runSection(parent context.Context, s *Section, set Settings, stopAll context.CancelFunc) SectionResult {
	sr := SectionResult{Name: s.Title()}
	items := sectionActions(s)
	has := func(name string) bool {
		for _, it := range items {
			if it.Name == name {
				return true
			}
		}
		return false
	}
	rc, hasRerun := rerunFor(items, set)

	// With rerun, exitnow waits for the reruns to run out; without it, exitnow
	// fires at the first failed ck.
	pr := runPass(parent, s, set, 0, has("exitnow") && !hasRerun)

	if hasRerun && !pr.aborted && !pr.skipped && len(pr.failures) > 0 {
		sr.RerunMax = rc.Retry
		for n := 1; n <= rc.Retry; n++ {
			if !sleepCtx(parent, rc.Sleep) {
				pr.aborted = true
				break
			}
			pr = runPass(parent, s, set, rc.Timeout, false)
			sr.Reruns = n
			if pr.aborted || pr.skipped || len(pr.failures) == 0 {
				break
			}
		}
	}

	sr.Stmts, sr.Skipped, sr.Aborted, sr.Failed = pr.stmts, pr.skipped, pr.aborted, len(pr.failures) > 0
	if pr.timedOut {
		sr.Notes = append(sr.Notes, fmt.Sprintf("TIMEOUT  the last pass was cut off after %s", rc.Timeout))
	}
	if pr.aborted || !sr.Failed {
		return sr
	}

	// At least one ck failed, and no rerun is left to fix it.
	sr.Actions = runEffects(parent, items, newReport(sr.Name, pr.failures), set)
	switch {
	case has("exitnow"):
		sr.Actions = append(sr.Actions, ActionOutcome{Name: "exitnow"})
		stopAll() // cancel every other section right now
	case has("exit"):
		sr.Actions = append(sr.Actions, ActionOutcome{Name: "exit"})
	}
	return sr
}
