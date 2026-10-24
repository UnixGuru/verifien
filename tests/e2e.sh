#!/usr/bin/env bash
# End-to-end tests against the real binary: builds it, starts tests/srv.py (an
# HTTP and UDP receiver), and runs config files through it, checking the
# report, the exit status, the timing and what the actions sent.
#
#   tests/e2e.sh
#   HTTP_PORT=19000 UDP_PORT=19001 SMTP_PORT=19002 tests/e2e.sh   if the default ports are taken
#
# Needs bash, go, python3, curl and awk. Takes about 40 seconds. Exits 1 if any
# check fails.
set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
HTTP_PORT=${HTTP_PORT:-18080}
UDP_PORT=${UDP_PORT:-15514}   # syslog; the same number is used over TCP
SMTP_PORT=${SMTP_PORT:-15525}
INC_PORT=${INC_PORT:-18081}   # a throwaway web server for the URL include check
RX="$WORK/rx"    # what the receiver saw
RUN="$WORK/run"  # the directory verifien runs in (it reads ./verifien.cfg)
BIN="$WORK/verifien"
mkdir -p "$RX" "$RUN"
FAILS=0
SRV_PID=""

cleanup() {
	[[ -n "$SRV_PID" ]] && kill "$SRV_PID" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

pass() { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; FAILS=$((FAILS + 1)); }

# check NAME COMMAND...   passes when the command succeeds
check() {
	local name=$1
	shift
	if "$@" >/dev/null 2>&1; then pass "$name"; else fail "$name"; fi
}
has() { [[ "$OUT" == *"$1"* ]]; }   # the report contains the text
lacks() { [[ "$OUT" != *"$1"* ]]; }
took_between() { awk -v t="$TOOK" -v lo="$1" -v hi="$2" 'BEGIN { exit !(t >= lo && t <= hi) }'; }
rx_count() { if [[ -f "$RX/http.log" ]]; then wc -l <"$RX/http.log" | tr -d ' '; else echo 0; fi; }
reset_rx() { rm -f "$RX/http.log" "$RX/syslog.log" "$RX/syslog-tcp.log" "$RX"/mail-*.eml; }

# run CONFIG   runs verifien with CONFIG as verifien.cfg; sets OUT, CODE, TOOK
run() {
	printf '%s\n' "$1" >"$RUN/verifien.cfg"
	local start end
	start=$(date +%s.%N)
	OUT=$(cd "$RUN" && "$BIN" --debug 2>&1)
	CODE=$?
	end=$(date +%s.%N)
	TOOK=$(awk -v s="$start" -v e="$end" 'BEGIN { printf "%.2f", e - s }')
}

FAILING='ck environ("VERIFIEN_SURELY_UNSET", set == yes)'
PASSING='ck environ("PATH", set == yes)'

echo "building"
(cd "$ROOT" && go build -o "$BIN" .) || { echo "build failed"; exit 1; }

python3 "$ROOT/tests/srv.py" "$HTTP_PORT" "$UDP_PORT" "$RX" "$SMTP_PORT" &
SRV_PID=$!
up=0
for _ in $(seq 50); do
	if curl -s -o /dev/null -X POST -d '{}' "http://127.0.0.1:$HTTP_PORT/up"; then up=1; break; fi
	sleep 0.1
done
[[ $up -eq 1 ]] || { echo "receiver did not start (ports in use? try HTTP_PORT=... UDP_PORT=... SMTP_PORT=...)"; exit 1; }
reset_rx

# ---------------------------------------------------------------------------
echo "config errors are reported with a line number and exit status 1"
# Each case is: expected text | config (\n for a newline)
errors=(
	'unknown action "boom"|[a]\n'"$FAILING"'\naction boom'
	'as:nope is not defined|[a]\n'"$FAILING"'\naction as:nope'
	'takes no options|[a]\n'"$FAILING"'\naction exit {retry 2}'
	'no url|[a]\n'"$FAILING"'\naction webhook'
	'no path|[a]\n'"$FAILING"'\naction logfile'
	'already given|[a]\n'"$FAILING"'\naction syslog, syslog'
	'can never run|[a]\nif environ("PATH", set == yes)\naction exit'
	'defined twice|[ACTION_SCRIPTS]\nas:x = "true"\nas:x = "true"\n[a]\n'"$FAILING"
	'unknown setting|[DEFAULTS] {bogus = 1}\n[a]\n'"$FAILING"
	'between 0 and 255|[DEFAULTS] {exit_status_bad = 300}\n[a]\n'"$FAILING"
	'invalid number|[DEFAULTS] {sleep = 1s}\n[a]\n'"$FAILING"
)
for e in "${errors[@]}"; do
	want=${e%%|*}
	cfg=${e#*|}
	run "$(printf '%b' "$cfg")"
	if [[ $CODE -eq 1 && "$OUT" == *"$want"* && "$OUT" == *"verifien.cfg:"* ]]; then pass "$want"; else fail "$want (exit $CODE: $OUT)"; fi
done

# ---------------------------------------------------------------------------
echo "every action fires, and gets the report"
reset_rx
rm -f "$WORK/stdin.json" "$WORK/fail.log"
printf '%s\n' "[DEFAULTS] {webhook_url = \"http://127.0.0.1:$HTTP_PORT/hook\",
otel_endpoint = \"http://127.0.0.1:$HTTP_PORT\",
otel_service = \"e2e-svc\",
syslog_address = \"udp://127.0.0.1:$UDP_PORT\",
syslog_tag = \"e2e\",
syslog_facility = local3,
logfile_path = \"$WORK/fail.log\"}

[ACTION_SCRIPTS]
as:rec = \"cat > $WORK/stdin.json\"

[web servers]
ck lstat(\"/nonexistent/a\", type == f)
ck environ(\"VERIFIEN_SURELY_UNSET\", {msg \"must be exported\"}, set == yes)
$PASSING
action as:rec, syslog, webhook, opentel, logfile, exit" >"$WORK/all-actions.cfg"
export OTEL_EXPORTER_OTLP_HEADERS='Authorization=Bearer%20tok'
run "$(cat "$WORK/all-actions.cfg")"
unset OTEL_EXPORTER_OTLP_HEADERS
sleep 0.3
check "exit status 1 (action exit)" test "$CODE" -eq 1
check "report lists the actions" bash -c "[[ '$OUT' == *'ACTION  as:rec'* && '$OUT' == *'ACTION  logfile'* && '$OUT' == *'ACTION  exit'* ]]"
check "script stdin: section, failures by ck, messages" python3 - "$WORK/stdin.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert d["program"] == "verifien" and d["section"] == "web servers", d
assert len(d["failures"]) == 2, d
assert d["failures"][0]["ck"].startswith('lstat("/nonexistent/a"'), d
assert d["failures"][1]["message"] == "must be exported", d
PY
check "script stdin: file, line and all the options of each failed ck" python3 - "$WORK/stdin.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
f0, f1 = d["failures"]
assert f0["file"] == "verifien.cfg" and f1["file"] == "verifien.cfg", d
assert f1["line"] == f0["line"] + 1 and f0["line"] > 0, d
assert f0["options"] == {"timeout": 10, "retry": 0, "sleep": 0}, d
assert f1["options"] == {"timeout": 10, "retry": 0, "sleep": 0, "msg": "must be exported"}, d
PY
check "--debug output shows file:line and the options" bash -c "[[ '$OUT' == *'FAIL  ck  verifien.cfg:'*'{timeout 10, retry 0, sleep 0}'* ]]"
check "logfile has one JSON line per failure" test "$(wc -l <"$WORK/fail.log" | tr -d ' ')" -eq 2
check "syslog: facility local3 + err, tag, section" bash -c "grep -q '<155>.*e2e.*section=\"web servers\"' '$RX/syslog.log'"
check "webhook got the report" python3 - "$RX/http.log" <<'PY'
import json, sys
reqs = [json.loads(l) for l in open(sys.argv[1])]
hook = [r for r in reqs if r["path"] == "/hook"]
assert len(hook) == 1 and json.loads(hook[0]["body"])["section"] == "web servers", reqs
PY
check "opentel: /v1/logs, service name, 2 ERROR records, bearer header" python3 - "$RX/http.log" <<'PY'
import json, sys
reqs = [json.loads(l) for l in open(sys.argv[1])]
otel = [r for r in reqs if r["path"] == "/v1/logs"]
assert len(otel) == 1 and otel[0]["auth"] == "Bearer tok", reqs
rl = json.loads(otel[0]["body"])["resourceLogs"][0]
assert rl["resource"]["attributes"][0]["value"]["stringValue"] == "e2e-svc"
recs = rl["scopeLogs"][0]["logRecords"]
assert len(recs) == 2 and all(r["severityText"] == "ERROR" for r in recs), recs
PY

# ---------------------------------------------------------------------------
echo "exit status: 0 by default, 1 for exit/exitnow, overridable in [DEFAULTS]"
run "[a]
$PASSING"
check "no failures -> 0" test "$CODE" -eq 0
run "[a]
$FAILING"
check "failed ck without an action -> 0" test "$CODE" -eq 0
run "[a]
$FAILING
action exit"
check "exit -> 1" test "$CODE" -eq 1
run "[a]
$FAILING
action exitnow"
check "exitnow -> 1" test "$CODE" -eq 1
run "[DEFAULTS] {exit_status_good = 7}
[a]
$PASSING"
check "exit_status_good = 7" test "$CODE" -eq 7
run "[DEFAULTS] {exit_status_bad = 20}
[a]
$FAILING
action exit"
check "exit_status_bad = 20" test "$CODE" -eq 20
run "[DEFAULTS] {exit_status_now = 30}
[a]
$FAILING
action exitnow"
check "exit_status_now = 30" test "$CODE" -eq 30

# ---------------------------------------------------------------------------
echo "msg replaces the built-in message; environ ignores retry, sleep and timeout"
run "[a]
ck environ(\"VERIFIEN_SURELY_UNSET\", {msg \"must be exported\", retry 9, sleep 5, timeout 1}, set == yes)"
check "msg shown instead of the built-in text" bash -c "[[ '$OUT' == *'-> must be exported'* && '$OUT' != *'is false'* ]]"
check "retry 9 / sleep 5 ignored (finished at once)" took_between 0 1

# ---------------------------------------------------------------------------
echo "actions run in goroutines, each with its own timeout, retry and sleep"
run "[ACTION_SCRIPTS]
as:a = \"sleep 2\"
as:b = \"sleep 2\"
as:c = \"sleep 2\"
[x]
$FAILING
action as:a, as:b, as:c"
check "three 2s scripts finish in about 2s, not 6s" took_between 1.9 3.5

reset_rx
run "[a]
$FAILING
action webhook {url \"http://127.0.0.1:$HTTP_PORT/slow\", timeout 1, retry 1, sleep 0.3}"
check "hanging webhook: 2 requests (1 + retry 1)" test "$(rx_count)" -eq 2
check "...in about 2.3s" took_between 2.2 4
check "...failure shown, exit status untouched" bash -c "[[ '$OUT' == *'ACTION  webhook  FAILED: timed out after 1s'* && $CODE -eq 0 ]]"

reset_rx
run "[a]
$FAILING
action webhook {url \"http://127.0.0.1:$HTTP_PORT/slow\", timeout 0.5}"
check "a fractional timeout (0.5 s) stops a hanging webhook" bash -c "[[ '$OUT' == *'timed out after 500ms'* ]]"
check "...in well under a second and a half" took_between 0.4 1.5

reset_rx
run "[DEFAULTS] {retry = 2,
sleep = 0.1}
[a]
$FAILING
action webhook {url \"http://127.0.0.1:$HTTP_PORT/err\"}"
check "HTTP 500 retried with retry from [DEFAULTS]: 3 requests" test "$(rx_count)" -eq 3
reset_rx
run "[DEFAULTS] {retry = 4}
[a]
$FAILING
action webhook {url \"http://127.0.0.1:$HTTP_PORT/err\", retry 0}"
check "the action's own retry 0 beats [DEFAULTS] retry 4: 1 request" test "$(rx_count)" -eq 1

rm -f "$WORK/childpid"
run "[ACTION_SCRIPTS]
as:hang = \"sleep 60 & echo \$! > $WORK/childpid; wait\"
[a]
$FAILING
action as:hang {timeout 1}"
check "hung script is cut off at its timeout" has "FAILED: timed out after 1s"
sleep 0.5
# A killed process whose parent is gone can sit as a zombie until init reaps it
# (some containers never do), so "gone" means no process or state Z.
is_dead() {
	local state
	state=$(ps -o stat= -p "$1" 2>/dev/null | tr -d ' ')
	[[ -z "$state" || "$state" == Z* ]]
}
check "...and its child process is killed too" is_dead "$(cat "$WORK/childpid")"

# ---------------------------------------------------------------------------
echo "rerun"
rm -f "$WORK/late"
(
	sleep 1
	touch "$WORK/late"
) &
LATE_PID=$!
reset_rx
run "[a]
ck lstat(\"$WORK/late\", type == f)
action rerun {retry 4, sleep 0.6}, webhook {url \"http://127.0.0.1:$HTTP_PORT/hook\"}, exit"
wait "$LATE_PID"  # only this job: the receiver in the background never exits
check "file appears after 1s: passes on rerun 2 of 4" has "RERUN  passed on rerun 2 of 4"
check "...so no action fired and the status is 0" bash -c "[[ $CODE -eq 0 && $(rx_count) -eq 0 && '$OUT' != *ACTION* ]]"

rm -f "$WORK/never"
reset_rx
run "[DEFAULTS] {rerun_retry = 2,
rerun_sleep = 0.2}
[a]
ck lstat(\"$WORK/never\", type == f)
action rerun, webhook {url \"http://127.0.0.1:$HTTP_PORT/hook\"}, exit
[b]
ck lstat(\"$WORK/never\", type == f)
action rerun {retry 1}"
check "rerun_retry from [DEFAULTS]: still failing after 2 reruns" has "still failing after 2 rerun(s) (max 2)"
check "section b's own retry 1 beats [DEFAULTS]" has "still failing after 1 rerun(s) (max 1)"
check "the actions of section a ran once, after the reruns" test "$(rx_count)" -eq 1
check "exit status 1" test "$CODE" -eq 1

# ---------------------------------------------------------------------------
echo "exitnow stops every other section; exit does not"
run "[fast]
$FAILING
action exitnow
[slow]
ck lstat(\"$WORK/never\", {retry 30, sleep 1}, type == f)"
check "the slow section is aborted, not waited for" has "ABORT"
check "...in well under its 30s of retries" took_between 0 3
check "...with exit status 1" test "$CODE" -eq 1

run "[fast]
$FAILING
action exit
[slow]
ck lstat(\"$WORK/never\", {retry 2, sleep 0.5}, type == f)"
check "exit lets the other section finish its retries (about 1s)" took_between 0.9 3
check "...and it is not marked aborted" lacks "ABORT"

# ---------------------------------------------------------------------------
echo "syslog over tcp, and email"
reset_rx
run "[DEFAULTS] {syslog_address = \"tcp://127.0.0.1:$UDP_PORT\",
syslog_tag = \"e2etcp\",
smtp_server = \"127.0.0.1:$SMTP_PORT\",
smtp_from = \"verifien@example.com\",
smtp_to = \"ops@example.com\",
smtp_tls = none}
[web]
ck environ(\"VERIFIEN_SURELY_UNSET\", {msg \"must be exported\"}, set == yes)
action syslog, email"
sleep 0.3
check "tcp syslog line arrived" bash -c "grep -q '<27>\\|<11>\\|<19>\\|<3>' '$RX/syslog-tcp.log' && grep -q 'e2etcp' '$RX/syslog-tcp.log'"
check "one mail arrived" test "$(ls "$RX"/mail-*.eml 2>/dev/null | wc -l | tr -d ' ')" -eq 1
check "mail has subject and message" bash -c "grep -q '^Subject: verifien: 1 failed check(s) in \[web\]' '$RX'/mail-1.eml && grep -q 'must be exported' '$RX'/mail-1.eml"
check "failed ck alone still exits 0" test "$CODE" -eq 0

# ---------------------------------------------------------------------------
echo "include, sinclude and stdout"
mkdir -p "$RUN/inc"
printf '[DEFAULTS] {exit_status_bad = 42}\n[included]\n%s\naction stdout\n' "$FAILING" >"$RUN/inc/part.cfg"
run "[DEFAULTS] {retry = 0}
include \"inc/part.cfg\"
sinclude \"inc/not-there.cfg\"
[main]
$PASSING"
check "include + sinclude of a missing file load fine" test "$CODE" -eq 0
check "stdout action printed the JSON report" bash -c "[[ '$OUT' == *'\"section\":\"included\"'* ]]"
run "include \"inc/not-there.cfg\"
[main]
$PASSING"
check "include of a missing file fails with exit 1" test "$CODE" -eq 1
check "...and names the file and line" has "verifien.cfg:1"
run "include \"inc/part.cfg\"
[DEFAULTS] {exit_status_bad = 5}
[x]
$FAILING
action exit"
check "the including file's [DEFAULTS] wins: exit status 5" test "$CODE" -eq 5
reset_rx
printf '[fromurl]\n%s\naction stdout\n' "$FAILING" >"$RX/team.cfg"
(cd "$RX" && python3 -m http.server "$INC_PORT" --bind 127.0.0.1 >/dev/null 2>&1 & echo $! >"$WORK/inc.pid")
sleep 0.7
run "include \"http://127.0.0.1:$INC_PORT/team.cfg\"
sinclude \"http://127.0.0.1:$INC_PORT/nope.cfg\""
kill "$(cat "$WORK/inc.pid")" 2>/dev/null
check "include of a URL works" bash -c "[[ '$OUT' == *'\"section\":\"fromurl\"'* ]]"
check "sinclude of a URL that 404s is skipped" test "$CODE" -eq 0

# ---------------------------------------------------------------------------
echo "--debug and --cfg"
printf '[x]\n%s\naction exit\n' "$FAILING" >"$RUN/verifien.cfg"
OUT=$(cd "$RUN" && "$BIN" 2>&1); CODE=$?
check "without --debug nothing is printed" test -z "$OUT"
check "...and the exit status is the same (1)" test "$CODE" -eq 1
OUT=$(cd "$RUN" && "$BIN" --debug 2>&1); CODE=$?
check "with --debug the report is printed" has "FAIL"
printf '[from-cfg]\n%s\n' "$PASSING" >"$WORK/other.cfg"
OUT=$(cd "$RUN" && "$BIN" --debug --cfg "$WORK/other.cfg" 2>&1); CODE=$?
check "--cfg PATH replaces verifien.cfg completely" bash -c "[[ '$OUT' == *'[from-cfg]'* && '$OUT' != *'[x]'* && $CODE -eq 0 ]]"
printf '[from-url]\n%s\n' "$PASSING" >"$RX/remote.cfg"
(cd "$RX" && python3 -m http.server "$INC_PORT" --bind 127.0.0.1 >/dev/null 2>&1 & echo $! >"$WORK/inc.pid")
sleep 0.7
OUT=$(cd "$RUN" && "$BIN" --debug -cfg "http://127.0.0.1:$INC_PORT/remote.cfg" 2>&1); CODE=$?
kill "$(cat "$WORK/inc.pid")" 2>/dev/null
check "--cfg URL works" bash -c "[[ '$OUT' == *'[from-url]'* && '$OUT' != *'[x]'* ]]"
OUT=$(cd "$RUN" && "$BIN" --debug --cfg "$WORK/nope.cfg" 2>&1); CODE=$?
check "--cfg of a missing file fails with exit 1" test "$CODE" -eq 1
OUT=$(cd "$RUN" && "$BIN" --bogus 2>&1); CODE=$?
check "an unknown option fails with exit 2" test "$CODE" -eq 2

# ---------------------------------------------------------------------------
echo "! negation and the stat items"
touch "$WORK/perm-file"; chmod 640 "$WORK/perm-file"
ln -sf perm-file "$WORK/perm-link"
mkfifo "$WORK/perm-fifo" 2>/dev/null || true
run "[a]
ck ! lstat(\"$WORK/shouldnotexist\", type == f)
ck ! environ(\"VERIFIEN_SURELY_UNSET\", set == yes)
ck lstat(\"$WORK/perm-file\", type == f, size == 0, nlink == 1, perm == 0640, perm == \"u=rw,g=r,o=\", perm == \"-0440\", perm == \"+0040\", perm != \"+0007\")
ck lstat(\"$WORK/perm-file\", uid == $(id -u), gid == $(id -g), uid == $(id -un))
ck lstat(\"$WORK/perm-link\", type == l, link == \"perm-file\")
ck lstat(\"$WORK/perm-fifo\", type == p)
ck lstat(\"/dev/null\", type == c)
action exit"
check "all negation and stat checks pass (exit 0)" test "$CODE" -eq 0
check "...every statement printed PASS" bash -c "[[ '$OUT' != *FAIL* ]]"
run "[a]
ck ! lstat(\"$WORK/perm-file\", type == f)
ck lstat(\"$WORK/perm-file\", perm == 0600)
action exit"
check "a failing ! and a wrong perm fail (exit 1)" test "$CODE" -eq 1
check "...the message shows the mode in octal" has "(got 0640)"

echo "variable items as a target and as a value"
mkdir -p "$WORK/real-dir"; chmod 755 "$WORK/real-dir"
ln -sfn "$WORK/real-dir" "$WORK/dir-link"
touch "$WORK/same-a" "$WORK/same-b" "$WORK/other-c"; chmod 640 "$WORK/same-a" "$WORK/same-b"; chmod 600 "$WORK/other-c"
run "[a]
l = lstat(\"$WORK/dir-link\")
ck l.type == l
ck lstat(l.link, type == d, perm == 0755)
x = lstat(\"$WORK/same-a\")
ck lstat(\"$WORK/same-b\", perm == x.perm, uid == x.uid)
ck lstat(\"$WORK/other-c\", perm == x.perm)
action exit"
check "a link's target is looked at, equal perms compare equal (exit 1 for the last)" test "$CODE" -eq 1
check "...the report shows the references" has "lstat(l.link, type == d"
check "...and the failing comparison" has "perm == x.perm"
run "[a]
x = lstat(\"/\")
ck lstat(x.size, type == d)"
check "a non-text item as a target is a config error" test "$CODE" -eq 1
check "...and says so" has "is not text"
run "[a]
x = lstat(\"/\")
ck lstat(\"/\", perm == x.uid)"
check "comparing a perm with a uid is a config error" test "$CODE" -eq 1
check "...and says so" has "cannot be compared"

echo "prometheus"
run "[a]
ck prometheus(\"http://127.0.0.1:$HTTP_PORT/metrics/?name=node_cpu_info\", package == 0)
ck prometheus(\"http://127.0.0.1:$HTTP_PORT/metrics?node_cpu_info\", package == 1, core == 0)
ck ! prometheus(\"http://127.0.0.1:$HTTP_PORT/metrics?node_cpu_info\", package == 1, core == 1)
action exit"
check "prometheus labels match, trailing slash tolerated (exit 0)" test "$CODE" -eq 0
check "...every statement printed PASS" bash -c "[[ '$OUT' != *FAIL* ]]"
run "[a]
ck prometheus(\"http://127.0.0.1:$HTTP_PORT/metrics?node_cpu_info\", package == 1, core == 1)
action exit"
check "prometheus with no matching series fails (exit 1)" test "$CODE" -eq 1
check "...and says which series were looked at" has "none of the 3 series of node_cpu_info has package == 1, core == 1"

echo "sed"
mkdir -p "$WORK/sed-root/private/etc"; chmod 755 "$WORK/sed-root/private/etc"
ln -sfn private/etc "$WORK/sed-root/etc"
run "[a]
v = lstat(\"$WORK/sed-root/etc\")
ck v.type == l
v2 = sed(v.link, \"s/^/\\//\", \"s#^#$WORK/sed-root#\")
ck lstat(v2.value, type == d, perm == 0755)
p = lstat(\"$WORK/perm-file\")
q = sed(p.perm, \"s/^/-/\")
ck lstat(\"$WORK/perm-file\", perm == q.value)
action exit"
check "sed turns a relative link target into a path and a permission into a find mode (exit 0)" test "$CODE" -eq 0
check "...every statement printed PASS" bash -c "[[ '$OUT' != *FAIL* ]]"
run "[a]
v = sed(\"x\", \"s/a\")"
check "a bad sed command is a config error" test "$CODE" -eq 1
check "...and says so" has "unterminated"

echo "cmd"
run "[a]
ck cmd(\"echo hello; echo oops >&2; exit 2\", exitstatus == 2, stdout == \"hello\", stderr == \"oops\")
ck ! cmd(\"exit 1\", exitstatus == 0)
r = cmd(\"echo viaVariable\")
ck r.stdout == \"viaVariable\"
action exit"
check "cmd exit status, stdout, stderr and a variable (exit 0)" test "$CODE" -eq 0
check "...every statement printed PASS" bash -c "[[ '$OUT' != *FAIL* ]]"
run "[a]
ck cmd(\"sleep 30\", {timeout 1, msg \"the command hung\"})
ck cmd(\"echo after\", stdout == \"after\")
action exit"
check "a hung command is stopped after the timeout and the section goes on (exit 1)" test "$CODE" -eq 1
check "...with the msg" has "the command hung"
check "...within about a second, not 30" took_between 0.9 5
check "...and the next check still ran" has "PASS"
rm -f "$WORK/cmd-marker"
run "[a]
ck cmd(\"if [ -e $WORK/cmd-marker ]; then echo second; else touch $WORK/cmd-marker; sleep 30; fi\", {timeout 1, retry 1, sleep 0.2}, stdout == \"second\")
action exit"
check "a hung first attempt is retried (exit 0)" test "$CODE" -eq 0
rm -f "$WORK/cmd-count"
run "[DEFAULTS] {retry 3}
[a]
ck cmd(\"echo x >> $WORK/cmd-count; exit 1\", exitstatus == 1)
action exit"
check "cmd ignores [DEFAULTS] retry: one run" test "$(wc -l <"$WORK/cmd-count")" -eq 1
rm -f "$WORK/cmd-count"
run "[a]
ck cmd(\"echo x >> $WORK/cmd-count; exit 1\", {retry=2}, exitstatus == 0)
action exit"
check "a non-zero status is retried: 3 runs" test "$(wc -l <"$WORK/cmd-count")" -eq 3
check "...and still fails when every run fails (exit 1)" test "$CODE" -eq 1
rm -f "$WORK/cmd-count"
run "[a]
ck cmd(\"echo x >> $WORK/cmd-count; exit 0\", {retry=2})
action exit"
check "a zero status is not retried: 1 run" test "$(wc -l <"$WORK/cmd-count")" -eq 1

echo "stat follows links, lstat does not"
run "[a]
ck lstat(\"$WORK/dir-link\", type == l)
ck stat(\"$WORK/dir-link\", type == d, perm == 0755)
ck ! stat(\"$WORK/dir-link\", type == f)
action exit"
check "lstat sees the link, stat sees the directory (exit 0)" test "$CODE" -eq 0
run "[a]
ck stat(\"$WORK/dir-link\", link == \"x\")"
check "stat has no link item (config error)" test "$CODE" -eq 1
check "...and says so" has "has no test"

echo "type e and \$?"
run "[a]
ck lstat(\"$WORK/perm-file\", type == e, type == f)
ck lstat(\"$WORK/no-such-file\", type != e)
ck ! stat(\"$WORK/dir-link\", type != e)
ck cmd(\"exit 3\", \$? == 3)
action exit"
check "type == e / != e and \$? work (exit 0)" test "$CODE" -eq 0
check "...every statement printed PASS" bash -c "[[ '$OUT' != *FAIL* ]]"

echo
if [[ $FAILS -eq 0 ]]; then
	echo "all checks passed"
else
	echo "$FAILS check(s) FAILED"
	exit 1
fi
