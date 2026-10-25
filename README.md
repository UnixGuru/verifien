# verifien

`verifien` reads a small configuration file, runs the checks in it, reports what
passed and what failed, and can act on the failures: write a log, call a webhook,
send mail, run a script, or exit with a status. It is one static Go binary with no
runtime dependencies, meant for cron jobs, monitoring hooks and "is this machine
set up the way I think it is?" checks.

The configuration language is a small DSL, parsed with
[participle](https://github.com/alecthomas/participle). Each section of the file
runs in its own goroutine, and every check that can block (a hung NFS mount, a slow
exporter, a stuck command) has a timeout, retries and a pause between retries.

```
[web host]
ck lstat("/etc/nginx/nginx.conf", type == f, perm == 0644, uid == root)
ck cmd("systemctl is-active nginx", {timeout 5, retry=2, sleep 1}, stdout == "active")
ck prometheus("http://localhost:9100/metrics?node_load1")
action syslog, webhook {url "https://hooks.example.com/alerts"}, exit
```

**Unix only** (Linux, macOS and the BSDs). It is not built for Windows or Plan 9.

## Contents

* [Quick start](#quick-start)
* [A tour of the language](#a-tour-of-the-language)
* [Functions](#functions)
* [Actions](#actions)
* [Running it](#running-it)
* [Building and testing](#building-and-testing)
* [Layout of the repository](#layout-of-the-repository)
* [Adding a function or an action](#adding-a-function-or-an-action)
* [More documentation](#more-documentation)

## Quick start

```
go mod tidy                  # once, to fill in go.sum
go build -o verifien .
```

Create `verifien.cfg` in the directory you run it from:

```
[basics]
ck lstat("/etc/passwd", type == f, size > 0)
ck lstat("/tmp", type == d, perm == "-1000")      # the sticky bit is set
action exit
```

```
$ ./verifien --debug
[basics]
  PASS  ck  verifien.cfg:2  lstat("/etc/passwd", type == f, size > 0)  {timeout 10, retry 0, sleep 0}
  PASS  ck  verifien.cfg:3  lstat("/tmp", type == d, perm == "-1000")  {timeout 10, retry 0, sleep 0}
$ echo $?
0
```

Actions only run when a check failed, so there is no `ACTION` line here. Make
`/tmp` fail (`type == f`) and `exit` runs and the status becomes 1.

Without `--debug` a run prints nothing at all (apart from configuration errors and
the `stdout` action), so it is quiet under cron. The exit status is the same either
way.

## A tour of the language

A file is a list of sections. A section starts with a header in square brackets
(any text) and runs its statements from top to bottom. `#` starts a comment.
Strings are `"double"` or `'single'` quoted.

| Statement | Meaning |
|---|---|
| `ck fn("target", item OP value, ...)` | a **check**: it fails if the call fails or any test is false |
| `if fn("target", item OP value, ...)` | a **guard**: if it does not pass, the section's `ck` statements are skipped |
| `var = fn("target")` | gather data into a variable; it gates nothing |
| `ck var.item OP value` | check an item of an earlier variable (`if` works the same way) |
| `ck ! fn(...)` | negation: passes when the call fails, errors, or any test is false |
| `action a, b {options}, c` | what to do when a `ck` failed |

```
[backup]
s = lstat("/mnt/backup/latest.tar")
ck s.size > 10M
ck s.name =~ /\.tar$/
ck ! lstat("/mnt/backup/.lock", type == e)         # no lock file
action rerun {retry 3, sleep 5}, logfile {path "/var/log/verifien.log"}
```

**Operators** are `==  !=  <  <=  >  >=  =~  !~`; the ones an item accepts depend on
its type (numbers, sizes such as `3K` `10M` `2T`, text, regular expressions like
`/\.db$/`, users and groups by name or number, permissions like `find -perm`).

**Options** go in a `{ }` block right after the target: `{timeout 0.5, retry 2,
sleep 1, msg "custom failure text"}`. The `=` is optional (`retry=2`). Each option
comes from the call, else the `[DEFAULTS]` block, else the built-in value (`cmd` is
the exception: it never retries unless the call says so).

```
[DEFAULTS] {
  timeout 3,
  retry   2
}
```

**Variables can feed later calls.** An item of a variable can be the target string of
a later call, or the value in a test, which is how you compare things between
inodes or turn a relative link into a path:

```
[links]
l = lstat("/etc")
ck l.type == l
p = sed(l.link, "s,^,/,")                           # private/etc -> /private/etc
ck stat(p.value, type == d, perm == 0755)

a = lstat("/srv/a")
ck lstat("/srv/b", perm == a.perm, uid == a.uid)    # b is like a
```

**Includes** splice other files (or URLs) in where they are written:
`include "common.cfg"` fails if the file is missing, `sinclude "local.cfg"` skips a
missing one.

**Reports** carry the line of every statement. With `--debug` each line shows
`file:line` and **all** the options the function ran with. Failures sent to actions
carry `file`, `line` and `options` as well.

The complete syntax, every option and the grammar are in [syntax.md](syntax.md).

## Functions

| Function | Looks at | Items you can test |
|---|---|---|
| `lstat("path")` | a path, **not** following symlinks (like `os.Lstat`) | `type` (`f d l b c p s`, and `e` for "exists"), `size`, `nlink`, `link`, `uid`, `gid`, `perm`, `name` |
| `stat("path")` | a path, following symlinks (like `os.Stat`) | the same, without `l` and `link` |
| `prometheus("URL?metric")` | a Prometheus text-format page | any label name: `package == 0, mode != "idle"` |
| `cmd("one-line command")` | the exit status and output of `sh -c` | `exitstatus` (also `$?`), `stdout`, `stderr` |
| `sed("text", "s/a/b/", ...)` | a piece of text, changed with `s` and `y` commands | `value` |
| `environ("NAME")` | an environment variable | `set`, `value` |
| `disk("path")` | placeholder: not implemented yet, always an error | |

A few things worth knowing:

* `type == e` means "the inode exists, of any type" and `type != e` means "it does
  not exist", so `lstat("/tmp/x.lock", type != e)` is the natural "must not be there".
* `prometheus` passes when at least one series of the metric has **all** the labels
  you list. A trailing slash on the URL path is dropped, as exporters dislike
  `/metrics/`.
* `cmd` treats exit status 0 as success. A non-zero status, a command that cannot
  start, one killed by a signal and one that runs past its timeout are failed
  attempts, and are retried only when the call has `{retry N}`. A hung command and
  everything it started are killed at the timeout. A command runs when its own
  statement is reached, never ahead of an earlier `if`.
* `sed` handles several commands (one per string, or separated by `;`), with the
  expressions written as in `sed -E`. It takes any item as its text, so a permission
  becomes `0640` and a size its byte count.

## Actions

Actions run when a section has a failed `ck`, after the section finishes.

| Action | Does |
|---|---|
| `exit` | exit with `exit_status_bad` (default 1) once everything has run |
| `exitnow` | stop all sections at once and exit |
| `rerun` | run the section again (with retries and a pause) before the other actions |
| `as:NAME` | run a one-line script from `[ACTION_SCRIPTS]` with `sh -c`; the failure report is on its standard input as JSON |
| `syslog` | write to the local syslog daemon, or a remote one |
| `webhook` | POST the JSON report |
| `logfile` | append one JSON line per failure |
| `opentel` | send an OpenTelemetry log record (HTTP and JSON) |
| `email` | one mail per section over SMTP with STARTTLS |
| `stdout` | print the report as one JSON line, even without `--debug` |

The JSON every action receives, for each failed `ck`:

```json
{ "ck": "cmd(\"sleep 5\", exitstatus == 0)", "message": "timed out after 2s",
  "file": "verifien.cfg", "line": 12,
  "options": { "timeout": 2, "retry": 0, "sleep": 0 } }
```

A failed action never changes the exit status, and a failed `ck` on its own exits 0:
only `exit` and `exitnow` change it, so a monitoring cron job can choose.

## Running it

```
verifien                              # reads ./verifien.cfg
verifien --cfg /etc/verifien/web.cfg  # or a URL: --cfg https://example.com/team.cfg
verifien --debug                      # also print the report
```

| Exit status | Meaning |
|---|---|
| 0 | normal, including failed checks when no `exit` action ran |
| `exit_status_bad` (1) | an `exit` action ran, or Ctrl-C |
| `exit_status_now` (1) | an `exitnow` action ran |
| 1 | a problem in the configuration file (reported on standard error) |
| 2 | a bad command line |

The SMTP password is never read from the config file: set `VERIFIEN_SMTP_PASSWORD`.

## Building and testing

You need Go 1.24 or newer.

```
go mod tidy                       # once: fills in go.sum
go build -o verifien .
go vet ./...
go test -race ./...               # unit tests (testify)
bash tests/e2e.sh                 # end-to-end: builds the binary, runs real configs
```

`tests/e2e.sh` needs `bash`, `go`, `python3`, `curl` and `awk`, and takes under a minute.
It starts `tests/srv.py`, a small local receiver for HTTP (webhook, include-by-URL, a
fake exporter), UDP and TCP syslog, and SMTP, all on `127.0.0.1`. The default ports are
18080, 18081, 15514 and 15525; if they are taken, set `HTTP_PORT`, `UDP_PORT`,
`SMTP_PORT` and `INC_PORT` (see the top of the script).

All `_test.go` files use [testify](https://pkg.go.dev/github.com/stretchr/testify).

## Layout of the repository

```
main.go, actions.go, include.go   the grammar, the validator, the runner, the report
funcs.go                          blank imports that link the functions and actions in
spec/                             the shared contracts: FuncSpec, ActionSpec, Attempt
                                  (goroutine + timeout + retry + sleep), the one
                                  implementation of "item OP value" testing
inode/    lstat, stat             metrics/  prometheus
shell/    cmd                     text/     sed
env/      environ                 disk/     disk (placeholder)
action_*/                         one folder per action
tests/                            e2e.sh and srv.py
syntax.md                         the user documentation and grammar reference
notes.md                          notes for people working on the code
```

Everything outside `spec/` and the two big files is a plugin: it registers itself
from `init()`, and `funcs.go` imports the folder.

## Adding a function or an action

A function is a folder with a Go file whose `init()` calls `spec.Register`:

```go
func init() {
	spec.Register("hello", spec.FuncSpec{
		Run:     hello,
		Options: spec.StdOptions(),                       // timeout, retry, sleep, msg
		Tests:   map[string]spec.Field{"greeting": {Kind: spec.KString}},
	})
}

func hello(ctx context.Context, req spec.Request) (spec.Result, error) {
	// run blocking work through spec.Attempt so the timeout and retries apply
	return spec.Result{"greeting": "hello " + req.Target}, nil
}
```

Add one `_ "verifien/yourfolder"` line to `funcs.go`. The runner takes care of parsing,
validating `item OP value` against the `Tests` table, options precedence, reports and
actions. Actions work the same way with `spec.RegisterAction`. [notes.md](notes.md)
explains the contracts, the lexer and grammar rules, and the details of each feature.

## More documentation

* [syntax.md](syntax.md): the configuration language, every option and setting, each
  function and action, the JSON report, and the grammar.
* [notes.md](notes.md): design notes for developers.
