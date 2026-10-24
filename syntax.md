# verifien: configuration syntax

`verifien` reads a file named `verifien.cfg` from the current directory, runs the
checks in it, reports what passed and failed, and can act on failures (log them,
call a webhook, run a script, exit with a status).

```
verifien                # reads ./verifien.cfg
```

A problem in the file itself (syntax, an unknown function, a bad option) is
reported with its line and column before anything runs, and the program exits
with status 1.

## A first example

```
[DEFAULTS] {
  timeout 3,                    # seconds (decimals allowed: 0.5), for every function and action
  retry   2                     # extra attempts after an error
}

[config files]
if environ("APP_ENV", value == "prod")        # only check prod
ck lstat("/etc/app.conf", type == f, size > 0)
ck lstat("/var/log/app", {msg "log directory is missing"}, type == d)
action syslog, exit

[backup]
s = lstat("/mnt/backup/latest.tar")
ck s.size > 10M
action rerun {retry 3, sleep 5}, webhook {url "https://hooks.example.com/x"}
```

## Running verifien

```
verifien                      # reads ./verifien.cfg
verifien --cfg /etc/verifien/web.cfg
verifien --cfg https://example.com/team.cfg
verifien --debug              # also print the report
```

| Option | Meaning |
|---|---|
| `--cfg PATH-OR-URL` | Use this config file (or `http(s)` URL) **instead of** `./verifien.cfg`. The default file is not read at all, so it does not even have to exist. Its `include` lines are relative to it. `-cfg` and `--cfg=PATH` work too. |
| `--debug` | Print the report: each section, `PASS`/`FAIL`/`SKIP` lines, `RERUN`, `TIMEOUT`, `ABORT` and the `ACTION` lines. |

**Without `--debug` nothing is printed by the run itself.** The exit status is the
same either way. Always shown, because they are asked for:

* errors in the config or on the command line, on standard error (exit status 1,
  or 2 for a bad command line);
* the output of the `stdout` action.

Everything else (the report, and the `ACTION name FAILED: reason` lines of actions
that failed) needs `--debug`. Use an action that records somewhere (`logfile`,
`syslog`, `webhook`...) and `--debug` while you are setting it up.

An `include` or `sinclude` may appear as many times as you like in a file, and the
files are read in the order they are written.

## The file

| Part | Written as | Notes |
|---|---|---|
| Section | `[any text you like]` | Everything up to the next header belongs to it. |
| Comment | `# to the end of the line` | Not a comment inside a string or a header. |
| String | `"double"` or `'single'` | `\\` `\"` `\'` `\n` `\t` `\r` mean what they say; any other `\x` stays as written, so `"s/^/\//"` and `"\d+"` work as typed. |
| Special blocks | `[DEFAULTS]`, `[ACTION_SCRIPTS]` | Any case. At most one of each per file. |
| Include | `include "file-or-url"`, `sinclude "file-or-url"` | Between sections. See [Including other files](#including-other-files). |

Each section runs in its own goroutine, so sections run at the same time.
Variables belong to the section that assigns them; they are not shared.

## `[DEFAULTS]`

Written like the options of a call: **one `{ }` block** of comma-separated
`key value` pairs (the `=` is optional). The braces are required, the block may
span lines and hold comments, and `{}` is allowed. Everything is optional; a key
that is missing takes the compiled-in value.

```
[DEFAULTS] {
  timeout 5,
  retry = 2,                    # = is optional
  syslog_facility local3
}
```

Options are always written inside `{ }`: here, on function calls
(`lstat("/x", {timeout 3}, ...)`) and on actions (`action webhook {url "..."}`).

| Key | Meaning | Compiled-in |
|---|---|---|
| `timeout` | Seconds one attempt may take, with decimals (`0.5`, `2.5`), the same type as `sleep`. Must be above 0. | `10` |
| `retry` (or `retries`) | Extra attempts after an error. | `0` |
| `sleep` | Seconds (`0.5`, `2`) to wait between attempts. | `0` |
| `rerun_retry` | How many times `action rerun` runs a section again. | `1` |
| `rerun_sleep` | Seconds to wait before each rerun. | `0` |
| `rerun_timeout` | Seconds one rerun pass may take (decimals allowed). Above 0. | none |
| `exit_status_good` | Exit status when no `exit`/`exitnow` action fired. | `0` |
| `exit_status_bad` | Exit status after `action exit`. | `1` |
| `exit_status_now` | Exit status after `action exitnow`. | `1` |
| `webhook_url` | Default URL for `action webhook`. | none |
| `logfile_path` | Default file for `action logfile`. | none |
| `otel_endpoint` | OpenTelemetry collector, e.g. `"http://collector:4318"`. | none |
| `otel_service` | `service.name` sent with the logs. | `verifien` |
| `syslog_tag` | Tag on syslog messages (also an option of `action syslog`). | `verifien` |
| `syslog_facility` | `user`, `daemon`, `local0` to `local7`, ... | `user` |
| `syslog_address` | `"udp://host:514"` or `"tcp://host:514"`. | local syslog |
| `smtp_server` | Mail server for `action email`, as `"host:port"`. | none |
| `smtp_from` | Sender address. | none |
| `smtp_to` | Default recipients, comma separated. | none |
| `smtp_user` | Login name. Turns on login (AUTH PLAIN). | none |
| `smtp_tls` | `starttls` (must be offered), `tls` (from the start) or `none`. | `starttls` |

The SMTP password is never in the file: set the environment variable
`VERIFIEN_SMTP_PASSWORD`.

The exit statuses must be between 0 and 255.

**Order of precedence** for `timeout`, `retry` and `sleep`: the option on the call
or action itself, then `[DEFAULTS]`, then the compiled-in value.

## Including other files

```
include  "web.cfg"                       # an error if the file is not there
sinclude "local.cfg"                     # skipped, silently, if the file is not there
include  "https://example.com/team.cfg"  # a URL works too (http:// or https://)
```

`include` and `sinclude` stand between sections, on their own. The included text
is read where the include is, so its sections run as if you had written them
there, in that order.

* **Paths** are relative to the file that contains the include (not to the
  directory you run verifien from). Included files may include others. An include
  loop is an error.
* **`sinclude`** skips only what is *not there*: no such file, or an HTTP 404/410.
  A file that exists but has a syntax error, a permission problem, a URL that
  answers 500 or cannot be reached are all still errors.
* **`[DEFAULTS]`** are merged. For a key given in more than one file, the including
  file wins over the files it includes, and a later include wins over an earlier
  one. Each file may still have only one `[DEFAULTS]` block.
* **`[ACTION_SCRIPTS]`** are merged too, and the same `as:IDENT` in two files is an
  error.
* **Errors** name the file and line they are in, so a mistake in an included file
  points at that file.
* **URLs**: each is fetched once at startup, with a 10 second limit and a 1 MiB
  size limit. A URL relative include (`include "more.cfg"` inside a file fetched
  from `https://host/dir/team.cfg`) is fetched from `https://host/dir/more.cfg`.
  A file fetched from a URL can include only URLs, never a local file.
  **A config can run commands (`as:` scripts), so only include URLs you trust, and
  prefer `https://`.**
* `include` and `sinclude` are reserved words, like `if`, `ck` and `action`.

## Statements

A section holds any number of these, in any order:

```
variable = name("string", {options})          # gather data; gates nothing
if name("string", {options}, test, test, ...) # a guard
ck name("string", {options}, test, test, ...) # a check
if ! name("string", ...)                       # ! turns if or ck around
if variable.item OP value {options}            # a guard on a gathered value
ck variable.item OP value {options}            # a check on a gathered value
action name, name {options}, ...               # what to do when a ck fails
```

`if` and `ck` are reserved words, and so is `action`.

### Negation with `!`

`if` and `ck` accept a `!` before the call or the check, for **every function**.
The statement then passes when the original would **not**: the call fails or
errors, or any of its tests is false.

```
if ! lstat("/shouldnotexist", type == f)          # only when that file is absent
ck ! environ("DEBUG_MODE", set == yes)           # DEBUG_MODE must not be set
ck ! lstat("/etc/nologin")                        # the file must not exist
s = lstat("/var/log/app.log")
ck ! s.size > 1G                                 # on a variable: "not larger than 1G"
ck ! environ("DEBUG_MODE", {msg "unset DEBUG_MODE"}, set == yes)
```

An error counts as "not true": `! lstat("/missing")` **passes** because `stat` fails,
and so does `! disk()` while `disk` is only a placeholder. When the `!` fails, the
message is `the statement is true, but the ! says it must not be`, or your `msg`.
The report shows the `!` in front of the statement.

### Function calls

```
name("string", {options}, item OP value, item OP value, ...)
name()                                           # for a function that needs no target
```

* The **string** is what the function looks at: a path, a variable name. It may
  also be an item of an earlier variable, see
  [Using a variable's items](#using-a-variables-items).
* The **`{options}`** block is optional. Inside it, the `=` is optional too:
  `{timeout 3, retry = 2, msg "custom text"}`.
* Each **test** is `item OP value`. The items a function offers are listed under
  [Functions](#functions) below.
* A test written directly in an assignment is an error, because it would gate
  nothing. Put the test on an `if` or a `ck`.

### Options every function accepts

| Option | Meaning |
|---|---|
| `timeout N.N` | Seconds one attempt may take; decimals are fine (`timeout 0.5`), as for `sleep`. Must be above 0. |
| `retry N` | Extra attempts after an error. The function runs once, plus all its retries, and only when those are used up does it fail. |
| `sleep N.N` | Seconds between attempts. |
| `msg "text"` | Replaces the function's built-in failure message with yours. |

A function that never blocks (such as `environ`) accepts `timeout`, `retry` and
`sleep` but ignores them, so the same options can be written on any call.

### Operators and values

| Operator | Meaning | Works on |
|---|---|---|
| `==` `!=` | equal, not equal | everything |
| `<` `<=` `>` `>=` | compare | numbers, sizes, text |
| `=~` | matches a regex | text |
| `!~` | does not match a regex | text |

* **Numbers**: `3`, `42`.
* **Sizes**: a number with a suffix, powers of 1024: `10K`, `3M`, `1G`.
* **Text**: a quoted string, `"hello"`.
* **Enums**: a bare word, `f`. Some items only allow `==` and `!=`.
* **Regular expressions**: `/^abc.*\.txt$/` or a quoted string (then backslashes
  must be doubled: `"\\d+"`). The syntax is Go's (RE2).

### Variables

```
s = lstat("/var/data/db")
if s.type == f
ck s.size < 3G
ck s.name =~ /\.db$/
```

A variable is assigned once, before it is used, and only in its own section. Its
items are the tests of the function that made it. A failed assignment is not a
failed check; a check on its variable then fails with "has no result".

A check on a variable (`if` or `ck`) may end with an `{options}` block. The only
option that does anything there is `msg`, which replaces the failure message:

```
etc = lstat("/etc/", {retry = 0})
ck etc.type == d {msg = "/etc/ is not a directory"}
if etc.size > 0 {msg "never mind"}
```

Any other option in that block is **ignored, not an error**: a check on a variable
runs no function, so `timeout`, `retry` and `sleep` have nothing to act on. Put them
on the call that made the variable. The `=` is optional, as everywhere.

### Using a variable's items

An item of an earlier variable can stand in for the string of a later call, or for
the value of a test. This works for every function.

```
l = lstat("/etc")
ck l.type == l                                   # /etc is a symlink...
ck lstat(l.link, type == d, perm == 0755)         # ...to a directory (l.link is the string)

a = lstat("/srv/a")
ck lstat("/srv/b", perm == a.perm, uid == a.uid, gid == a.gid)   # b is like a
ck b.size >= a.size                              # on a variable: b = lstat("/srv/b") first
```

* **As the string:** only an item of type text can be used (`link`, `name`,
  `value`, ...). A number, size, permission, user, group or enum item (`size`,
  `perm`, `uid`, `type`, ...) is a config error, except for [`sed`](#functions),
  which takes any item (a permission is its octal text, `0640`; a size is its byte
  count).
* **As a value:** the item must be the same kind of thing as the one it is compared
  with: a permission with a permission, a user with a user, a group with a group,
  an enum with an enum. Sizes and plain numbers may be compared with each other.
  Text can be compared with anything: a text test takes the text of any item, and a
  text item (such as the result of `sed`) can be the value of any test. That text is
  checked when the check runs. Text that looks like a number (`0640`, `4096`, `3K`)
  counts as a number, and `root` or `-0640` as text, so
  `perm == q.value` works when `q.value` is `-0640`. Text that does not fit
  fails the check with "is not usable here". Any other pair is a config error, as
  is an operator the item does not allow. A permission from a variable is compared
  like `== 0640` (exactly those bits).
* The variable must be assigned earlier in the same section, and the function that
  made it must offer that item.
* The call starts when its statement is reached, after the variable has finished
  (it is not started early with the other calls). If the variable failed, the
  statement fails with "has no result". `link` is `""` for something that is not a
  symlink, so `lstat(l.link)` then has an empty string to look at.
* The report shows the reference as written: `lstat(l.link, type == d)`.

### `if` and `ck`

* Every `if` in a section runs first. If any `if` fails, **no `ck` in the section
  runs** and the report shows `SKIP`. That is not a failure.
* A section can have zero or more `if`.
* If all the `if` pass, the `ck` run. A failed `ck` is reported as `FAIL`.

A failed `ck` does not change the exit status on its own. Only the actions below
do that.

## Functions

### `lstat("path", ...)` and `stat("path", ...)`

Both look at an inode. They differ only in what they do with a symbolic link, like
the system calls of the same names:

* **`lstat`** looks at the path itself. A symbolic link is reported as a link
  (`type == l`), with its `size` and `perm` as the link's own, and `link` says
  where it points. Even a link whose target is missing can be looked at.
* **`stat`** follows symbolic links (any number of them) and describes what they
  lead to: the target's type, size, owner and permissions. It never reports
  `type l` and has **no `link` item**, so `type == l` and `link ...` are config
  errors for `stat`, which tells you to use `lstat`. If the target is missing (a
  dangling link) or the links loop, `stat` fails; `ck ! stat("/path")` then passes.
  `name` is the last part of the path as you wrote it, not of the target.

Use `stat` to ask "is `/etc` a directory, whatever it is made of", and `lstat` to
ask "is `/etc` a link, and to where". The items are otherwise the same:

| Item | Type | Operators | Meaning |
|---|---|---|---|
| `type` | enum `f` `d` `l` `b` `c` `p` `s` `e` (`stat` has no `l`) | `==` `!=` | the letters `ls -l` shows: `f` file, `d` directory, `l` symbolic link, `b` block special, `c` character special, `p` fifo (named pipe), `s` socket. **`e`** is not a type: `type == e` means "the inode exists, whatever it is" and `type != e` means "it does not exist" |
| `size` | size | all comparisons | bytes. Write `0`, `512`, `3K`, `3M`, `3G`, `3T`; every step is 1024 times the one before (`1T` is 1024G), so you can test from zero bytes to many terabytes |
| `nlink` | number | all comparisons | number of hard links to the inode |
| `link` | text | all, and regex | **`lstat` only.** Where a symbolic link points, exactly as stored (relative or absolute, even if the target is missing). `""` if the path is not a link |
| `uid` | user | `==` `!=` | owner: a number or a login name, `uid == 0`, `uid == root`, `uid == "www-data"` |
| `gid` | group | `==` `!=` | group: a number or a group name |
| `perm` | permissions | `==` `!=` | permission bits, tested like `find -perm`; see below |
| `name` | text | all, and regex | the last part of the path |

**Does it exist?** `type == e` passes when the path exists (of any type) and `type != e`
passes when it does not. They combine with other tests without error:

```
ck lstat("/etc/app.conf", type == e, type == f, perm == 0600)   # exists, and is a regular file
ck lstat("/tmp/app.lock", type != e)                            # must not be there
ck ! lstat("/tmp/app.lock", type == e)                          # the same, the other way round
```

A missing path is an error for every other test (`type == d`, `size == 0`, ...), so
those fail with "no such file or directory". Only a `type != e` test in the call
turns "missing" into an answer: it is then not retried either. With `stat`, a
symbolic link whose target is missing counts as missing; with `lstat` the link
itself exists.

A user or group **name** is looked up when the check runs, so a config shared
between machines still loads where the account does not exist; there the test
simply fails with `no user "name"`.

**`perm`** works like `find`'s `-perm`:

| Written | True when |
|---|---|
| `perm == 0644` | the bits are exactly these |
| `perm == "-0644"` | at least all of these bits are set (more is fine) |
| `perm == "+0111"` | any one of these bits is set |
| `perm != ...` | the opposite of any of the above |

The mode is **octal** (only the bits in 07777 count: setuid 4000, setgid 2000,
sticky 1000, and the rwx bits) or **symbolic** as for `chmod`, such as
`"u=rw,go=r"`, `"a+x"` or `"u=rwx,g=rx,o="`. A symbolic mode starts from zero and
ignores the umask: `a+x` is `0111`, and `=rw` (no who) is `0666`. A leading `-` or `+`
is the find operator, so a symbolic mode cannot itself start with a dash.
Write any mode that has a `-`, a `+`, a comma or an `=` as a quoted string; a plain
octal number does not need quotes. In a failure, the mode found is shown in octal.

```
ck lstat("/etc/passwd", type == f, size > 0, uid == root, perm == 0644)
ck lstat("/etc/shadow", perm == "u=rw,g=r,o=")          # exactly rw-r-----
ck lstat("/usr/bin/su", perm == "-04000")                # setuid is set
ck lstat("/var/www/upload", perm != "+0002")             # nobody else can write
ck lstat("/var/log/app.log", size < 2T, nlink == 1)
ck lstat("/etc/localtime", type == l, link =~ /zoneinfo/)
ck lstat("/run/app.sock", type == s)
ck lstat("/dev/sda", type == b)
```

### `environ("NAME", ...)`

Looks at an environment variable. It never waits, makes one attempt, and ignores
`timeout`, `retry` and `sleep`.

| Item | Type | Operators | Meaning |
|---|---|---|---|
| `set` | enum `yes` `no` | `==` `!=` | whether the variable exists (set but empty is `yes`) |
| `value` | text | all, and regex | its value (empty if unset) |

```
ck environ("HOME", set == yes)
ck environ("APP_ENV", value == "prod", {msg "APP_ENV must be prod"})
```

### `prometheus("URL?metric_name", label OP value, ...)`

Fetches a page in the [Prometheus text format](https://prometheus.io/docs/instrumenting/exposition_formats/)
(what `node_exporter` and most exporters serve at `/metrics`) and passes when **at
least one series** of that metric has all the labels the tests describe.

```
ck prometheus("http://localhost:19100/metrics?node_cpu_info", package == 0)
ck prometheus("http://localhost:19100/metrics?name=node_cpu_info", package == 0, core == 3)
ck prometheus("http://localhost:19100/metrics?node_load1")          # the metric just has to exist
ck ! prometheus("http://localhost:19100/metrics?node_cpu_info", package == 7)
```

* **The target** is the page URL followed by `?` and the metric name. The form
  `?name=metric_name` also works; the `name` parameter is not sent. Other
  parameters stay (`?collect[]=cpu&name=node_cpu_info`). A trailing slash on the
  path is dropped, because exporters answer `/metrics` but not `/metrics/`. Only
  `http://` and `https://` are accepted.
* **Each test** is a label name, an operator and a value. Any label name is
  accepted (there is no fixed list, so a misspelt label is not caught until the
  check finds no series). The operators are `==`, `!=`, `=~` and `!~`; label values
  are text, so `package == 0`, `package == "0"` and `package == a.value` all compare
  the text `0`. A number is compared as written (`le == 0.5`, not `le == .5`).
* **All the tests must hold for the same series.** In the example above, a series
  with `package="1"` and another with `core="3"` do not together satisfy
  `package == 1, core == 3`. A label the series does not have counts as `""`, as in
  PromQL, so `mode == ""` and `mode != "idle"` work on a series with no `mode`.
* **It fails** when the page cannot be read (connection, timeout, a status other than
  200, a line of that metric that is not valid), when there is no metric of that
  name, and when no series matches. The message says which.
* It takes the standard options. `timeout` (default 10 s) and `retry` apply to
  fetching the page. The page is read up to 32 MiB. Both the plain form (`name{a="b"} 1`) and
  the quoted UTF-8 form (`{"metric.name","a b"="c"}`) are understood.
* It reports no items of its own, so a variable made by `prometheus(...)` has
  nothing to check; use it directly in `if`/`ck`. Its labels can take a text item of a
  variable as the value (`package == e.value`), see
  [Using a variable's items](#using-a-variables-items).
* Options go right after the string: `prometheus("URL?m", {timeout 3}, a == 1)`.

### `sed("text", "command", "command", ..., tests)`

Changes a piece of text, with the commands of `sed`, and reports the result as the
item `value`. It does not touch any file. It is how you reshape what one function
found before giving it to another.

```
var = lstat("/etc")
ck var.type == l
ck var.link == "private/etc"                     # a link relative to /
var2 = sed(var.link, "s/^/\//")                  # -> /private/etc
ck lstat(var2.value, type == d, perm == 0755)

x = lstat("/srv/a")
q = sed(x.perm, "s/^/-/")                        # 0640 -> -0640 (all of these bits)
ck lstat("/srv/b", perm == q.value)
ck sed("a-b-c", "y/-/_/", "s/_/-/2", value == "a_b-c")
```

* **The text** is a string or an item of a variable. For `sed` any item will do: a
  number or permission is used as its text (`0640`, `4096`, `1000`).
* **The commands** are strings that follow the text, applied in order, like several
  `-e` options. A string may also hold several commands separated by `;` or a
  newline (`"s/a/b/; s/c/d/"`). Only the commands that change text exist:
  * `s/regex/replacement/flags`. Any character except a backslash, a newline, a
    letter or a digit can be the delimiter (`s|/a|/b|`, `s,x,y,`); `\` followed by
    the delimiter means the delimiter itself. The flags are `g` (every match), `i`
    (ignore case) and a number (only that match; with `g`, that match and later
    ones), as in `s/a/b/2g`. In the replacement `&` is the whole match,
    `\1` to `\9` are groups, `\&` is a plain `&`, `\n` a newline, `\t` a tab.
  * `y/abc/xyz/`: each character of the first list becomes the one in the same place
    in the second (`\n`, `\\` and `\<delimiter>` are understood).
* **The expressions** are Go's (RE2), written like `sed -E` (extended): groups are
  `(...)` and not `\(...\)`, and `+ ? | {}` work without a backslash. Because a quoted string
  keeps unknown escapes, `"s/\d+/N/"` and `"s/\./,/g"` need no doubled backslashes.
* A command that cannot be read (unknown command, no closing delimiter, a bad
  expression, `\3` with only two groups) is a config error with the line number,
  so it never surprises a run. There is no "no match" error: text that does not
  match comes back unchanged. Test with `value == ...` if you need to know.
* It never waits, so `timeout`, `retry` and `sleep` have no effect; `msg` does.
* The one item is `value`, text. In a test or a later call it is used like any text
  item, see [Using a variable's items](#using-a-variables-items).
* Not provided: addresses (`/x/s/a/b/`, `2s/a/b/`), `d p a i c w`, `-n`, and the
  POSIX "basic" syntax of the expressions.

### `cmd("command", ...)`

Runs a one-line command with `sh -c` and tests how it ended and what it wrote.

```
ck cmd("systemctl is-active nginx", exitstatus == 0, stdout == "active")
ck cmd("df -P /var | awk 'NR==2 {print $5}'", {timeout 5}, stdout =~ /^[0-8]?[0-9]%$/)
ck cmd("pgrep -x sshd", {timeout 3, retry=2, sleep 1, msg "sshd is not running"}, exitstatus == 0)
ck ! cmd("grep -q ERROR /var/log/app.log", exitstatus == 0)
ck cmd("test -s /var/run/app.pid", $? == 0)                         # $? is exitstatus
```

| Item | Type | Operators | Meaning |
|---|---|---|---|
| `exitstatus` | number | `==` `!=` `<` `<=` `>` `>=` | the exit status, 0 to 255. **`$?`** is another name for it, as in the shell: `$? == 0` |
| `stdout` | text | `==` `!=` `<` `<=` `>` `>=` `=~` `!~` | what the command wrote to standard output |
| `stderr` | text | the same | what it wrote to standard error |

* The command is **one line**, run by `sh -c`, so pipes, `&&`, `$(...)` and quoting
  work as in a shell. Its standard input is empty (`/dev/null`) and it inherits
  verifien's environment and working directory. The command is the string, so it
  may also be an item of a variable (`cmd(v.value)`).
* **Trailing newlines** are dropped from `stdout` and `stderr`, as in `$(command)`,
  so `stdout == "active"` matches `active\n`. Only the first 1 MiB of each is kept.
* **Exit status 0 is success; anything else is a failed attempt.** A non-zero exit
  status, a command that cannot be started (not found is the shell's exit 127), one
  killed by a signal, and one that runs past the timeout are all retried. Status 0
  is never run again.
* **The default is no retry, and `[DEFAULTS] retry` does not apply to `cmd`.** A command
  can have effects, so it is run again only when the call asks: `cmd("script", {retry=2})`
  runs it up to 3 times, stopping at the first exit status 0, with `sleep` seconds
  between runs (`timeout` and `sleep` do come from `[DEFAULTS]` as for any function).
* **When the attempts run out,** a command that last exited non-zero is still a
  result, so `exitstatus == 3` can be tested (and a plain `exitstatus == 0` then fails
  with "got 3"). `exitstatus != 0` works the same way: it passes when the last run
  exited non-zero (and, with a retry, the command is run again until it exits 0 or the
  retries are used up). A command that was last stopped by the timeout, could not be started
  or was killed fails with that message. `msg` replaces the failure message.
* **A hung command is stopped.** Each attempt runs in its own goroutine. After
  `timeout` seconds (default 10) the command **and everything it started** are killed
  (they share a process group) and that attempt fails with "timed out"; the section
  carries on.
* It runs **when its own statement is reached**, one after another within a section,
  never early like `stat` or `prometheus`. A command can have effects, so an `if`
  above it that fails keeps it from running at all. Different sections still run
  at the same time.
* A variable made by `cmd(...)` has the items above: `r = cmd("..."); ck r.exitstatus == 0`.
  `r.stdout` can be the text for `sed` or the target of another call.
* Unix only.

### `disk("path", ...)`

Reserved for disk space checks (`free`, `used`, both sizes). It is not
implemented yet and reports "not implemented yet".

## Actions

```
action exit
action as:cleanup, syslog, exit
action rerun {retry 3, sleep 2}, webhook {url "https://hooks.example.com/x", timeout 5}
```

An `action` line is a comma-separated list. A section may have several `action`
lines; they add up. Each name appears at most once per section. An action needs
at least one `ck` in its section.

**When actions run.** After all the `ck` of the section have run, if at least one
failed (and no rerun is left to fix it). They do not run when an `if` skipped the
`ck`, or when everything passed.

**In what order.** First `rerun` (if any), then all the side-effect actions
(`as:`, `stdout`, `syslog`, `email`, `webhook`, `opentel`, `logfile`) **at the same time**, each in
its own goroutine, and last `exit` or `exitnow`.

**Failed actions.** If an action itself fails (the URL is down, the script exits
non-zero) the report shows `ACTION name FAILED: reason`. It does not change the
exit status.

### Options for actions that can hang

`webhook`, `opentel`, `syslog`, `email`, `logfile` and `as:` accept `timeout`, `retry` and
`sleep` with the same meaning as for functions, and the same precedence: the
action's own option, then `[DEFAULTS]`, then compiled-in.

### `exit`

Every `ck` in every section still runs. When all sections have finished and the
report is printed, the program ends with `exit_status_bad` (default 1). Takes no
options.

### `exitnow`

Stops at once. The first failed `ck` of the section triggers it (the rest of the
section is not run). The section's other actions run, every other section is
cancelled and shows `ABORT`, and the program ends with `exit_status_now`
(default 1). Takes no options.

If a section has both `rerun` and `exitnow`, `exitnow` waits until the reruns are
used up. If several sections trigger different exits, `exitnow` wins.

### `rerun`

Runs the whole section again (its assignments, `if` and `ck`) when a `ck` fails,
before any other action.

| Option | Meaning | Default from |
|---|---|---|
| `retry N` | How many extra passes at most. | `rerun_retry`, then `1` |
| `sleep N.N` | Seconds before each rerun. | `rerun_sleep`, then `0` |
| `timeout N.N` | Seconds one **rerun** pass may take. | `rerun_timeout`, then none |

It stops as soon as a pass has no failed `ck`. If it does, no action fires and
the report says `RERUN passed on rerun 2 of 3`. If the reruns are used up, the
last pass is reported, the other actions run once, and the report says
`RERUN still failing after 3 rerun(s)`. The first pass is not limited by the
rerun `timeout`; the functions' own timeouts apply to it. Every section can set
its own `rerun` options, so one section can differ from the rest.

### `as:IDENT`

Runs a one-line shell script from `[ACTION_SCRIPTS]`:

```
[ACTION_SCRIPTS]
as:ident1 = "logger -t verifien failed"
as:notify = "jq -r '.failures[].message' | mail -s 'verifien' ops@example.com"

[stanza a]
ck lstat("/etc/app.conf", type == f)
action as:ident1, exit
```

`IDENT` is an ordinary identifier: letters, digits and `_`, not starting with a
digit. The script receives a [JSON report](#the-json-report) on **standard
input**. Nothing from the report is put on the command line, so a hostile
message cannot inject shell commands. If the script times out, it and the
processes it started are killed. A non-zero exit status is a failure and its
output is shown in the report.

Scripts are shell commands from your own file, with your own privileges; treat
the file like any script.

### `syslog`

Sends one error-level syslog message per failed `ck`:

```
section="web servers" ck="lstat(\"/etc/x\", type == f)" message="lstat /etc/x: no such file or directory"
```

Uses the local syslog by default, or a remote server given by `syslog_address`.
Three values control it. Each can be set in `[DEFAULTS]` **and** overridden for one
use inside the action's `{ }`; the option on the action beats `[DEFAULTS]`, which
beats the compiled-in value:

| Name | Meaning | Compiled-in |
|---|---|---|
| `syslog_tag` | Tag on the messages. | `verifien` |
| `syslog_facility` | `kern`, `user`, `mail`, `daemon`, `auth`, `syslog`, `lpr`, `news`, `uucp`, `cron`, `authpriv`, `ftp`, `local0` to `local7`. | `user` |
| `syslog_address` | `"udp://host:514"` or `"tcp://host:514"`. | the local syslog |

```
[DEFAULTS] {syslog_tag "verifien", syslog_facility local3, syslog_address "udp://loghost:514"}

[web servers]
ck lstat("/etc/app.conf", type == f)
action syslog                                        # tag verifien, local3, to loghost

[database]
ck lstat("/var/lib/db", type == d)
action syslog {syslog_tag "db", syslog_facility daemon}   # same server, other tag and facility

[backup]
ck lstat("/mnt/backup/latest.tar", type == f)
action syslog {syslog_address "tcp://other:514", timeout 3, retry 1}
```

`timeout`, `retry` and `sleep` work as for every action.

### `stdout`

Writes the [JSON report](#the-json-report) of the section to standard output as
**one line**, so it can be piped (`verifien | jq .`). It takes no options except
`timeout`, `retry` and `sleep`.

```
[web servers]
ck lstat("/etc/app.conf", type == f)
action stdout
```

The normal text report is also printed to standard output, after the JSON lines.

### `email`

Sends **one** mail per section (not one per failed `ck`), listing the section,
the time and every failure:

```
[DEFAULTS] {
  smtp_server "mail.example.com:587",
  smtp_from   "verifien@example.com",
  smtp_to     "ops@example.com, Me <me@example.com>",
  smtp_user   "robot"
}

[web servers]
ck lstat("/etc/x", type == f)
action email {subject "web servers are broken", to "oncall@example.com"}
```

Options: `to` (replaces `smtp_to`), `subject` (default
`verifien: N failed check(s) in [section]`), plus `timeout`, `retry`, `sleep`.
Unless `smtp_tls = none`, the connection must be encrypted; a server that does not
offer STARTTLS is an error and nothing is sent. Newlines in the subject are
flattened to one line.

### `webhook`

POSTs the [JSON report](#the-json-report) to a URL (`Content-Type:
application/json`). Any `2xx` answer is success.

```
action webhook {url "https://hooks.example.com/x"}
```

The URL is the action's own `url` option, or `webhook_url` from `[DEFAULTS]`. It
must be `http://` or `https://`.

Options: `url`, plus the standard action options `timeout` (seconds per attempt),
`retry` (extra attempts after a failure) and `sleep` (seconds between attempts).
A non-2xx answer, a connection error and a timeout all count as a failed attempt.
Each option written on the action beats the same key in `[DEFAULTS]`.

A full example, with a default URL and an action that overrides everything:

```
[DEFAULTS] {
  webhook_url "https://hooks.example.com/default",
  timeout     10,
  retry       0
}

[web servers]
ck lstat("/var/www/index.html", type == f)
ck lstat("/var/log/app.log", size < 100M)

# Post to a different URL, wait at most 5 s per attempt, try up to 4 times
# (the first try plus 3 retries), pausing 2.5 s between tries.
action webhook {url "https://hooks.example.com/web-alerts", timeout 5, retry 3, sleep 2.5}

[database]
ck lstat("/var/lib/db/data", type == d)

# No options: uses webhook_url, timeout and retry from [DEFAULTS].
action webhook
```

The receiver gets one POST per section whose `ck` failed, with a body like:

```
{"program":"verifien","time":"2026-10-14T01:59:00Z","section":"web servers",
 "failures":[{"ck":"lstat(\"/var/log/app.log\", size < 100M)","message":"size < 100M is false (got 214958080)"}]}
```

If every attempt fails, the line `ACTION  webhook  FAILED: ...` appears in the
report; the exit status is not affected.

### `logfile`

Appends one JSON line per failed `ck` to a file (created if needed):

```
{"time":"2026-10-14T05:56:23Z","program":"verifien","section":"web","ck":"lstat(...)","message":"..."}
```

The path is the action's own `path` option, or `logfile_path` from `[DEFAULTS]`.

### `opentel`

Sends one `ERROR` log record per failed `ck` to an OpenTelemetry collector over
OTLP/HTTP with JSON bodies (`POST <endpoint>/v1/logs`). gRPC and protobuf are not
supported. Each record has the message as its body and the attributes
`verifien.section` and `verifien.ck`.

Where it finds its settings, first match wins:

| What | Order |
|---|---|
| Endpoint | `otel_endpoint` in `[DEFAULTS]`; `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` (used as is); `OTEL_EXPORTER_OTLP_ENDPOINT` (`/v1/logs` is added) |
| Service name | `otel_service`; `OTEL_SERVICE_NAME`; `verifien` |
| Headers | `OTEL_EXPORTER_OTLP_HEADERS` and `OTEL_EXPORTER_OTLP_LOGS_HEADERS` (`k=v,k2=v2`) |

Keep tokens in the environment (the headers variable), not in `verifien.cfg`.

## The JSON report

What `as:` scripts get on stdin and `webhook` posts. One report per section,
failures in the order the `ck` ran:

```json
{
  "program": "verifien",
  "time": "2026-10-14T05:56:23Z",
  "section": "web servers",
  "failures": [
    { "ck": "lstat(\"/etc/app.conf\", type == f)",
      "message": "lstat /etc/app.conf: no such file or directory",
      "file": "verifien.cfg", "line": 12,
      "options": { "timeout": 10, "retry": 0, "sleep": 0 } },
    { "ck": "environ(\"APP_ENV\", {msg \"APP_ENV must be exported\", retry 2}, set == yes)",
      "message": "APP_ENV must be exported",
      "file": "verifien.cfg", "line": 13,
      "options": { "timeout": 10, "retry": 2, "sleep": 0, "msg": "APP_ENV must be exported" } }
  ]
}
```

`message` is the function's own message, or your `msg` option if you gave one.

* **`file` and `line`** say where the failed `ck` is in the config. `file` is the
  file the line is in, so a `ck` that came from an `include` names that file.
* **`options`** are **all** the options the function ran with, for any function,
  whether or not the config gave them: its own `{...}`, else `[DEFAULTS]`, else the
  built-in value (`cmd` has no default retry, so it shows `retry` 0 unless the call
  gave one). `timeout` and `sleep` are in seconds; `msg` appears only if one was
  given. A failed check on a variable (`ck v.size > 3`) calls no function and has no
  `options`.

A
section pass that ran out of time under `rerun {timeout N}` adds a failure with
`"ck": "(section)"`.

## The report and the exit status

This is what `--debug` prints. Without `--debug` it is not printed (the exit status
below is unchanged).

```
[web servers]
  PASS  if  verifien.cfg:11  environ("APP_ENV", value == "prod")  {timeout 10, retry 0, sleep 0}
  FAIL  ck  verifien.cfg:12  lstat("/etc/app.conf", type == f)  {timeout 10, retry 0, sleep 0}  -> lstat /etc/app.conf: no such file or directory
  RERUN  still failing after 2 rerun(s) (max 2)
  ACTION  syslog
  ACTION  webhook  FAILED: timed out after 5s
  ACTION  exit
```

| Line | Meaning |
|---|---|
| `PASS` / `FAIL` | a statement passed or failed. After the `if`/`ck` come `file:line` (where it is in the config; an included file shows its own name and line), the statement, and for a function call **all** the options it ran with (see `options` in the JSON report), then `->` and the reason if it failed. A check on a variable has no options. |
| `SKIP` | an `if` failed, so the `ck` did not run |
| `RERUN` | what `rerun` did |
| `ACTION` | an action ran; `FAILED:` says why it did not work |
| `ABORT` | cancelled by `exitnow` in another section, or by Ctrl-C |

Exit status:

| Situation | Status |
|---|---|
| `exitnow` ran | `exit_status_now` (1) |
| `exit` ran, or Ctrl-C | `exit_status_bad` (1) |
| otherwise, including failed `ck` without an action | `exit_status_good` (0) |
| a problem in the config file | 1 |

## Grammar reference

The complete syntax in one place. `|` is a choice, `( )` groups, `?` is optional,
`*` is zero or more, and quoted text is literal. Whitespace and newlines between
tokens are free, so a statement may span lines, and `#` starts a comment that runs
to the end of the line (not inside a string or a `[header]`).

```
file         = ( defaults | scripts | include | section )* ;

defaults     = "[DEFAULTS]"       options ;    # braces required
include      = ( "include" | "sinclude" ) STRING ;

scripts      = "[ACTION_SCRIPTS]" script* ;
script       = ACTIONREF "=" STRING ;          # as:ident1 = "one-line shell script"

section      = HEADER statement* ;             # [any text except ]

statement    = action
             | IDENT "=" call                  # variable = fn(...)
             | ( "if" | "ck" ) "!"? ( call | check ) ;

call         = IDENT "(" ( target ( "," STRING )* ( "," options )? ( "," test )* )? ")" ;       # a test item may be any label name for prometheus
target       = STRING | IDENT "." IDENT ;      # lstat(l.link): a text item of an earlier variable
options      = "{" ( option ( "," option )* )? "}" ;
option       = IDENT "="? value ;              # {timeout 3, retry = 2, msg "x"}
test         = IDENT OPERATOR ( IDENT "." IDENT | value ) ;   # size < 3G, perm == a.perm
check        = IDENT "." IDENT OPERATOR ( IDENT "." IDENT | value ) options? ;   # var.size < 3G {msg "x"}

action       = "action" actionitem ( "," actionitem )* ;
actionitem   = ( ACTIONREF | IDENT ) options? ;

value        = STRING | NUMBER | IDENT | REGEX ;
```

Tokens:

| Token | Matches |
|---|---|
| `HEADER` | `[` any text without `]` or a newline `]`. `[DEFAULTS]` and `[ACTION_SCRIPTS]` (any case) are special headers. |
| `STRING` | `"..."` or `'...'`, with `\` escapes. |
| `NUMBER` | digits, an optional `.digits`, and an optional suffix: `3`, `0.5`, `3K`, `10G`, `2T`. |
| `IDENT` | a letter or `_`, then letters, digits or `_`. |
| `ACTIONREF` | `as:` followed by an `IDENT`. |
| `REGEX` | `/.../`, with `\/` for a slash. |
| `OPERATOR` | `==` `!=` `<` `<=` `>` `>=` `=~` `!~`. |
| punctuation | `( ) , = { } . !` |

Reserved words: `if`, `ck`, `action`, `include`, `sinclude`. Which names are valid for functions, options,
tests, actions and `[DEFAULTS]` keys, and which operators each test allows, are
checked after parsing; every error carries a line and column.

## Platform notes

* verifien is for Unix systems: Linux, macOS and the BSDs. It is not built for
  Windows or Plan 9.
* `as:` runs the script with `sh -c`.
* `syslog` uses the local daemon by default (`syslog_address` for a remote one).
* `opentel` is HTTP and JSON only.
