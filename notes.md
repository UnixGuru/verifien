# verifien: notes for programmers

How the code is laid out, how to add a **function** or an **action**, and the
things that are easy to get wrong. The user documentation is in `syntax.md`.

## Layout

```
verifien/
├── go.mod, go.sum            module "verifien"; participle/v2 (code), testify (tests only)
├── main.go                   lexer, grammar, validation, [DEFAULTS], one statement (exec), report, command line, main
├── include.go                include / sinclude: reading and merging other files and URLs
├── actions.go                action grammar and validation, running a section (passes, rerun), running actions
├── funcs.go                  blank imports: the only list of folders that are linked in
├── spec/                     types and code shared by the runner and every function/action folder
│   ├── spec.go               Kind, Field, Result, Request, Func, FuncSpec, Register, StdOptions
│   ├── attempt.go            Attempt: goroutine + timeout + retry + sleep
│   ├── check.go              the one implementation of "item OP value"
│   └── action.go             Report, ActionRequest, ActionSpec, RegisterAction
├── inode/stat.go             function: stat
├── disk/disk.go              function: disk (placeholder)
├── env/environ.go            function: environ
├── action_script/            action: as:IDENT (proc.go: sh -c, process-group kill)
├── action_syslog/            action: syslog (syslog.go: udp/tcp client; local.go: the local daemon)
├── action_stdout/            action: stdout (the JSON report, one line, on standard output)
├── action_email/             action: email (SMTP, STARTTLS by default)
├── action_webhook/           action: webhook
├── action_logfile/           action: logfile
├── action_opentel/           action: opentel
├── main_test.go, main_include_test.go, main_email_test.go, main_unix_test.go, */_test.go     go test
└── tests/e2e.sh, tests/srv.py                       black-box test of the real binary
```

Functions live in folders named by *type of thing* (`inode/`, `disk/`, `env/`).
Actions live in folders named `action_<name>/`. Code the runner and the plugins
share goes in `spec/`; a function or action folder never imports `main`.

## Running the tests

All `_test.go` files use [testify](https://pkg.go.dev/github.com/stretchr/testify@v1.12.1)
(`assert` to keep going after a failure, `require` to stop). After copying the
files, run `go mod tidy` once with normal internet access: it adds the
`go.sum` lines for testify's own dependencies. `t.Skip` stays plain `testing`.

```
go test -race ./...            # unit + in-process end-to-end, about 15 s
tests/e2e.sh                   # builds the binary and runs it against a local receiver, about 15 s
```

* A build with `-race` waits **one second at exit**. That is the race detector,
  not the code. `GORACE=atexit_sleep_ms=0` removes it.
* `main_test.go` registers test-only functions (`counter`, `blocker`, `hang`,
  `slowsecond`). Use them to make a section fail a set number of times, or block.
* Several tests check timing ("three 1 s scripts finish in under 1.8 s"). They
  have generous margins but can fail on a very loaded machine.
* `e2e.sh` starts `tests/srv.py` on ports 18080 (HTTP), 15514 (UDP and TCP syslog)
  and 15525 (SMTP). Override with `HTTP_PORT`, `UDP_PORT`, `SMTP_PORT`.
* `main_email_test.go` has a small fake SMTP server (`newSMTPServer`).
* Unix only (Linux, macOS, BSDs). Check macOS with `GOOS=darwin go build .`.
* `main_include_test.go` covers include/sinclude (files, URLs, merging) and `stdout`.

## How a run works

0. `main` parses the command line (`parseArgs`: `--cfg`, `--debug`) and calls
   `loadConfig`. `--cfg` (path or URL) replaces `./verifien.cfg` entirely; it is read
   with `readSource` (include.go), so a URL works. `report(results, set, debug)`
   renders to `io.Discard` unless `--debug`, so the exit status never depends on
   the flag. The `stdout` action and config errors are printed regardless.

1. `main` reads `verifien.cfg`, parses it with participle, then `File.Settings()`
   (the `[DEFAULTS]` and `[ACTION_SCRIPTS]` blocks), then `File.Validate(set)`.
   Everything that can be wrong in the file is reported here, with line:column.
2. `runAll` starts one goroutine per section (`runSection`).
3. `runSection` calls `runPass`, which runs the statements once:
   * phase 1: assignments and every `if`, in order. The first failing `if` ends
     the pass (`skipped`).
   * phase 2: every `ck`, in order.
   * Functions marked `Async` are started up front for each phase (futures keyed by
     `*Call`), so slow calls overlap; a statement waits only for its own result.
4. If a `ck` failed and the section has `rerun`, `runSection` sleeps and runs more
   passes. Then, if `ck` still fail, `runEffects` starts the side-effect actions,
   one goroutine each, and waits for all of them. Last come `exit`/`exitnow`.
5. `render` prints the results in file order and returns the exit status.

### Who enforces timeout, retry and sleep

**The runner does not.** It resolves them (`request()` for functions,
`buildRequest()` for actions: the call's own option, else `[DEFAULTS]`, else
compiled-in) and puts them in the `spec.Request` / `spec.ActionRequest`. The
function or action applies them to its blocking work with `spec.Attempt`. A
function that does not use `Attempt` is simply not protected. `environ` is the
example of one that does not need it.

`Attempt` runs each attempt in its own goroutine and abandons it at the timeout,
so a call that never returns **leaks that goroutine** (the caller is not held
up). Prefer a cancellable operation. `ActionRequest.Attempt` also gives the
operation a context that ends at the timeout, so an HTTP request or a process can
be stopped instead of left running; use it for actions.

### Exit status

`render` decides it: `exitnow` -> `exit_status_now`, `exit` or any aborted
section -> `exit_status_bad`, otherwise `exit_status_good`. A failed `ck`, and a
failed action, do not change it.

## Adding a function

A function answers "what is true about X?". It returns a `spec.Result`: item name
to value. The runner compares those against the `item OP value` tests from the
file, using the shared code in `spec/check.go`. **A function never evaluates the
tests itself.**

1. Pick or create a folder for the type of thing, e.g. `net/`.
2. Add a file with an `init()` that registers it:

```go
package net

import (
	"context"
	"net"

	"verifien/spec"
)

func init() {
	spec.Register("resolve", spec.FuncSpec{
		Run:     resolve,
		Async:   true,              // slow: start it as soon as the section starts
		Options: spec.StdOptions(), // timeout, retry, sleep, msg
		Tests: map[string]spec.Field{
			"count": {Kind: spec.KInt},
			"first": {Kind: spec.KString},
		},
	})
}

func resolve(ctx context.Context, req spec.Request) (spec.Result, error) {
	// A blocking call that cannot be cancelled goes through Attempt.
	addrs, err := spec.Attempt(ctx, req, func() ([]string, error) {
		return net.LookupHost(req.Target)
	})
	if err != nil {
		return nil, err // becomes the failure message
	}
	return spec.Result{"count": int64(len(addrs)), "first": addrs[0]}, nil
}
```

3. If the folder is new, add one blank import to `funcs.go`. Adding a function to
   an existing folder needs nothing else.
4. Add tests next to it (`resolve_test.go`).

Rules:

* **Value types in `Result`**: `KInt` and `KSize` items must be `int64`; `KString`
  and `KEnum` items must be `string`. Anything else is reported as a failure
  ("function returned X, want int64").
* `Kind`s: `KString`, `KInt`, `KSize` (3K/3M/3G), `KEnum` (give `Enum`), `KFloat`.
  `KFloat` is for options only; it cannot be used as a test item.
* `Field.Ops` narrows the operators a test item allows (`stat`'s `type` only takes
  `==` and `!=`). The kind sets the outer limit (`kindOps` in `check.go`).
* `Options` is exactly what may appear in `{}`. The runner only reads `timeout`,
  `retry`, `sleep` (into the `Request`) and `msg` (for the failure message). Any
  other option you add is validated (kind check) but reaches you only through the
  grammar; to use a new option you must read it yourself. Standard options belong
  in `spec.StdOptions()`.
* If a function genuinely never blocks, do not call `Attempt`, and still use
  `StdOptions()` so the options stay accepted (and ignored).
* Names are global. Registering a name twice panics at startup.
* Return promptly when `ctx` is cancelled.

## Adding an action

An action does something when a section's `ck` fail. It receives the
`spec.Report` (section and failures) and returns an error or nil.

1. Create `action_<name>/`.
2. Register it from `init()`:

```go
package actionslack

import (
	"context"

	"verifien/spec"
)

func init() {
	spec.RegisterAction("slack", spec.ActionSpec{
		Run: run,
		// Options allowed after the name, besides timeout/retry/sleep.
		Options: map[string]spec.Field{"channel": {Kind: spec.KString}},
		// [DEFAULTS] keys this action owns. Prefix them with the action name.
		Settings: map[string]spec.Field{"slack_token_file": {Kind: spec.KString}},
		// Optional: runs while the config is validated, before anything runs.
		Check: func(req spec.ActionRequest) error { return nil },
	})
}

func run(ctx context.Context, req spec.ActionRequest) error {
	channel := req.Opt("channel")             // this use's own option
	tokenFile := req.Setting("slack_token_file") // from [DEFAULTS]
	// req.Report.Section, req.Report.Failures ...
	return req.Attempt(ctx, func(actx context.Context) error {
		// do the blocking work with actx; it ends at req.Timeout.
		// The whole attempt is repeated on error, up to req.Retry times,
		// with req.Sleep between attempts.
		return nil
	})
}
```

3. Add the blank import to `funcs.go`.
4. Add tests in `main_test.go` style (a real `httptest.Server`, a temp file...) and a
   case in `tests/e2e.sh` if it talks to something outside the process.
5. Document it in `syntax.md` (the Actions section and the `[DEFAULTS]` table).

Rules:

* **Each action runs in its own goroutine**, started by `runEffects`, and the
  section waits for all of them. Do not start your own long-lived goroutines.
* **The action must use `req.Attempt`** (or `spec.Attempt`) for anything that can
  hang. The runner does not enforce a timeout for you.
* Do not retry by yourself, and do not make a failure silent: return the error.
  It is shown as `ACTION name FAILED: ...` and does not change the exit status.
* A `Settings` key name must be unique across all actions (`RegisterAction`
  panics otherwise). Values arrive as text in `req.Settings`, validated against
  their `Field` before anything runs.
* Do not put secrets in `[DEFAULTS]` if an environment variable can carry them;
  `opentel` reads the standard `OTEL_*` variables for that reason.
* The names `exit`, `exitnow` and `rerun` are built into the runner and cannot be
  registered. `as` is registered by `action_script`; the runner splits `as:IDENT`.
* Anything that is OS-specific goes in a file with a build tag and has a stub for
  the other systems (see `action_syslog/syslog_unix.go` and `syslog_other.go`).

## stat items, permissions and owners

* `inode/stat.go` fills `type nlink link uid gid perm size name` from one `Lstat`
  (plus `Readlink` for a link), inside `spec.Attempt`. It reads `syscall.Stat_t`, so it
  is Unix only. `nlink` is cast from a type that differs between Linux and macOS.
* New value kinds live in `spec`: `KUID`/`KGID` (`owner.go`: number or a name looked up
  with `LookupUser`/`LookupGroup`, replaceable in tests) and `KPerm` (`perm.go`:
  octal or chmod-symbolic, with the find `-`/`+` prefix). A function reports all of
  them as `int64`.
* `ParseSize` knows K M G T (powers of 1024) and refuses a size that overflows int64.
* `!` (negation) is in `exec`/`execInner` in `main.go`, not in the functions, so it
  works for every function, present and future.

## Grammar and lexer: things that bite

* Lexer rules are tried in order. `DefaultsHeader` and `ActionScriptsHeader` must
  come before `Header`; `Keyword` (`if|ck`), `Action` and `ActionRef` (`as:ident`)
  before `Ident`; `Regex` before `Operator`; `Operator` before `Punct` (so `==` is
  not two `=`). A `#` inside a string or a header is never a comment because those
  rules match whole.
* The grammar uses `UseLookahead(2)`: after `if`/`ck`, `fn(` and `var.` both start
  with an identifier, and after `,` a `{` (options) is told from an identifier
  (test) by the next token. In a call's parentheses and after an operator, `ident .`
  (a variable item) is told from a bare word (`f`, `yes`) the same way.
* Variable items as targets/values: `Call.TargetVar/TargetItem`, `Test.RefVar/RefItem`,
  `Check.RefVar/RefItem`. Validation (`checkTargetRef`, `validateTest`,
  `spec.ValidateRef`) needs the section's `vars` map, so `Call.Validate` takes it.
  A call with a variable target is not prefetched; `startCall` waits for the
  variable's future. Values cross over as text via `spec.FormatItem` (target) and
  `spec.ItemValue` (a `spec.Value`, so the ordinary `Compare` does the work; perm
  becomes octal `0640`). Targets must be `KString`; values need equal Kinds, with
  `KInt`/`KSize` interchangeable.
* `strconv`-style quoting: strings are unquoted by participle; a regex literal's
  slashes are stripped by a token map.
* The text of a statement in the report and in failures (`callText`) is rebuilt from
  the parse tree, so it is normalised (`lstat("/x", type == f)`), and it does not
  include the `{options}`.
* `go vet` complains about participle struct tags; that is expected.

## Decisions and known limits

* **Failed `ck` alone exits 0.** Only `exit`/`exitnow` set a failing status, as
  asked. Change `render` if that should be the default.
* **`rerun_timeout`/`rerun {timeout}` limits the reruns, not the first pass.** The
  functions' own timeouts bound the first pass.
* **`exitnow` with `rerun`** waits for the reruns; without `rerun` it fires at the
  first failed `ck` and the rest of the section is not run.
* **Actions of one section run concurrently**, so their order is not the written
  order, but the report lists them in written order.
* **`opentel` is OTLP/HTTP + JSON, written by hand.** The OpenTelemetry Go SDK
  could not be fetched in the environment this was built in (the
  `go.opentelemetry.io` host was blocked), so a small client is used instead. It
  is about 150 lines and needs no dependency, but it has no gRPC, no protobuf, no
  batching, and no compression. If the SDK becomes an option, `action_opentel`
  is the only folder to change; keep the `otel_*` settings and the `OTEL_*`
  environment fallbacks so existing files keep working.
* **Unix only.** Windows and Plan 9 support was removed on purpose; code may use
  `syscall` process groups and `log/syslog` freely.
* **`as:` runs `sh -c`** (`proc.go`), with the report as JSON on stdin and the
  script's whole process group killed at the timeout.
* **`syslog` has a hand-written RFC 3164 client** for udp/tcp (remote servers);
  `log/syslog` is used only for the local daemon (`local.go`).
* **Options always go in `{ }`.** `[DEFAULTS]` is one `{ }` block with the same
  `key value` syntax as call options (it reuses the `Options` grammar type).
* **Includes are expanded before anything is validated** (`include.go: expand`),
  producing one `File` with one merged `[DEFAULTS]` and one `[ACTION_SCRIPTS]`.
  Precedence: the including file beats its includes; a later include beats an
  earlier one. `sinclude` skips only "not there" (ENOENT, HTTP 404/410): widen
  `readSource`'s `missing` if you want it to skip network errors too. A file
  fetched from a URL can include only URLs. URL timeout (10 s) and size (1 MiB)
  are constants in `include.go` because `[DEFAULTS]` is not known yet while the
  includes are being read.
* **`email`**: one mail per section; STARTTLS required unless `smtp_tls = none`; the
  password comes only from `VERIFIEN_SMTP_PASSWORD`, never the file. The report is not put in the command
  line or the environment.
* **Zombies:** a killed script's children can show as zombies in containers whose
  PID 1 does not reap. They are dead; tests treat state `Z` as gone.
* `disk` is a placeholder. When it is implemented, keep `Async: true` and run the
  statfs-style call through `spec.Attempt`.
* A statement's `msg` replaces the function's message completely. The detail
  (`got 6`) is not appended.

## Ideas not done

* A `rerun`-style `skip`; a per-section `quiet` (behaviour not defined yet).
* gRPC for `opentel` via the SDK.
* Globs in `include` (`include "conf.d/*.cfg"`), and an `[DEFAULTS]` timeout for URL includes.
* Checking `if`/`ck` across sections, or sharing variables between them (sections
  are concurrent, so it would need an explicit dependency order).

## prometheus and label tests

* `spec.FuncSpec.Labels`: a function with it accepts any item name in a test; the item
  is a label (`spec.LabelField`: text, `Loose` so `package == 0` works, operators
  `== != =~ !~`). The runner does **not** evaluate label tests; `startCall` resolves
  them (a variable's item becomes its value) and passes them in `Request.Tests`,
  because they must all hold for the same series. `exec` skips them.
* A call whose target or test value refers to a variable (`Call.needsVars`) is not
  prefetched. `startCall` copies the variables it needs before the goroutine starts;
  the goroutine never reads `sectionRun.vars` (the section writes it).
* `metrics/prometheus.go`: `splitTarget` (URL, metric; drops the trailing slash and
  `name=`), `fetch` (inside `spec.Attempt`, per-attempt context timeout), `parse`
  (cheap prefix filter, then `parseSample` for the legacy and the quoted UTF-8
  forms), `matches` (a missing label is `""`; uses `spec.Evaluate`, so the operators
  behave as everywhere else).
* Not done: `value` or series-count tests, `__name__` as a label, and OpenMetrics-only
  syntax (a sample with an exemplar after the value is rejected, as is anything but
  `value [timestamp]`); the request asks for the plain text format.

## sed and strings

* `spec.FuncSpec.Args` (a checker for the strings after the target) and
  `Request.Args`; a function without `Args` rejects extra strings in `Call.Validate`.
  The checker runs at load, so `sed` compiles its commands then. `FuncSpec.TextInput`
  lets a function take any variable item as its target (`spec.FormatItem` text).
* Strings are unquoted by `unquoteString` in main.go, not participle's `Unquote`
  (which rejects `\/` and `\d`): `\\ \" \' \n \t \r` are processed, any other `\x`
  is kept with its backslash. A `/regex/` literal is untouched.
* `text/sed.go` is a small sed in Go (`s` with `g i N`, `y`, several per string),
  RE2 as the regex dialect (like `sed -E`). `parse` reads commands in order;
  `field` splits at unescaped delimiters.
* Typed values from text: `spec.ValidateRef` accepts text on either side of a
  comparison; `spec.PrepareWant` (called from `evalTest` for variable values)
  turns number-like text into a Number, then `CheckValue`s it against the tested
  item, so a bad value fails the check at run time ("is not usable here").

## cmd

* `shell/cmd.go`. `cmd` wraps one attempt (`once`) in `spec.Attempt`, which runs it in
  a goroutine and abandons it at the timeout; `once` also has a context with the
  timeout so `exec` kills the process group (`Setpgid`, `Cancel` = `kill(-pid)`,
  as in `action_script/proc.go`) and `WaitDelay` (2 s) stops a background child that
  holds the pipes from hanging `Run`.
* Deliberately **not** `Async`: prefetch would start the command before an earlier
  `if` decides the section's checks are skipped, and a command has effects.
* An attempt succeeds only on exit status 0; a non-zero status fails the attempt with a
  `*nonZero` error that carries the result, so `spec.Attempt` retries it and `cmd`
  turns the final one back into a normal result (`errors.As`). Timeouts, start
  failures and signal deaths stay errors. `FuncSpec.NoDefaultRetry` makes `request()` start
  from retry 0, so only the call's own `{retry N}` counts; `timeout` and `sleep` still
  come from `[DEFAULTS]`.
* Output is captured through `capped`: the first 1 MiB is kept, the rest is read and
  dropped so the command never blocks on a full pipe.

## stat and lstat

* `inode/stat.go` registers both from one `newSpec`. `look` is the shared body, given
  `os.Lstat` or `os.Stat`. `stat` omits the `link` item and the `l` type, so the
  usual item validation rejects them with "has no test"; its result has no `link`.
* A dangling link or a loop is an error for `stat` (the error from `os.Stat`), so it
  retries like any error and `ck ! stat(...)` passes.

## type e and $?

* `spec.Field.Any`: an enum value standing for "whatever was reported" (`== Any` is
  `got != ""`, `!= Any` is `got == ""`), handled in `Compare`. `stat`/`lstat` set it to
  `e`. `FuncSpec.WantsTests` passes all the call's tests in `Request.Tests` (as
  `Labels` does for labels); `look` uses it to see a `type != e` test and, if the path is
  missing, report `type: ""` as a result inside the attempt (so no retry) instead of
  an error. Other tests then fail with "did not report".
* `$?`: the `Ident` lexer rule also matches `$?`, and a token map rewrites it to
  `exitstatus`, so it is that item wherever an identifier is accepted (the report shows
  `exitstatus`). For a function without that item the error says there is no test
  `exitstatus`.

## Positions and effective options in the report

* `StmtResult.Pos` is the statement's position (file from `expand`, so an included
  statement keeps its own file and line). `render` prints `file:line`; `spec.Failure`
  gets `File`/`Line` from it.
* `StmtResult.Options` (`*spec.Options`) is `effectiveOptions(call, set)`: the
  `request()` the function got (call option > `[DEFAULTS]` > built-in, `NoDefaultRetry`
  applied) plus the call's `msg`. It is set for assignments, `if`/`ck` calls and not for
  checks on a variable. `Failure.Options` is the same, so every failing `ck` that
  called a function carries it. `Options.String()` is the `{timeout 2, retry 0, sleep 0}` form
  shown by `--debug`.

