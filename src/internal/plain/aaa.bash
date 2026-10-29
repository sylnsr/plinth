# The AAA library: the helpers of a plain AAA ask script, printed by plinthctl ask lib (this binary's own, so it always
# matches it and nothing on disk can drift). A script sources it in its one fixed line:
#   source <(plinthctl ask lib); [[ "${AAA_LOADED:-}" == 1 ]] || { echo "..." >&2; exit 2; }
# It checks the run before anything runs, writes the answer running.yml (apiVersion: plinth/v1, kind: Answer) through
# its helpers only, and its EXIT trap ends the run: failure and result, then the rename to answer[.FAIL].new.yml.
# plinthctl finish <RUN> then checks the document, runs the DLP scan and moves the run to done/. See plinthctl help aaa.
# Under the controller (_AAA_PLINTH=1) the controller writes it per run and names it $AAA_LIB (source "$AAA_LIB"): it has
# checked the run already and wrote the empty running.yml itself, and it scans the answer once the script has ended.
_AAA_BIN=@PLINTHCTL@   # the binary that printed this, for the text filter
_AAA_PLINTH=@PLINTH@   # 1: a run of the controller
set -uo pipefail

_aaa_refuse() { echo "${RUN_NAME:-the script}: $* - nothing was run." >&2; exit 2; }

# The script by its absolute path: it was started by any path and sources the library before any cd.
_AAA_SCRIPT="${BASH_SOURCE[1]:-}"
[[ -n "$_AAA_SCRIPT" && -f "$_AAA_SCRIPT" ]] || _aaa_refuse "the AAA library must be sourced by an ask script"
_AAA_DIR="$(cd "$(dirname "$_AAA_SCRIPT")" && pwd)" || _aaa_refuse "cd to the script's directory failed"
_AAA_SCRIPT="$_AAA_DIR/$(basename "$_AAA_SCRIPT")"
_AAA_ANSWER="$_AAA_DIR/running.yml"
_AAA_FAILURE=""   # set by fail
_AAA_SIGNAL=""    # under the controller: the signal that ended the run

[[ "${RUN_NAME:-}" =~ ^[a-z]+-[a-z]+(-[0-9]+)?$ ]] || _aaa_refuse "RUN_NAME \"${RUN_NAME:-}\" is not a run name (<adjective>-<noun>[-<issue>])"
[[ "${INTERACTIVE:-}" == 0 || "${INTERACTIVE:-}" == 1 ]] || _aaa_refuse "INTERACTIVE must be 0 or 1"
# INTERACTIVE=1: the script asks the user (ask_yes), so it is for the user's own terminal. AAA_QUIET=1 is how an agent
# runs a script through the silent IDE terminal tool, where nobody can answer: the two contradict each other.
[[ "$INTERACTIVE" == 1 && "${AAA_QUIET:-}" == 1 ]] && _aaa_refuse "contradictory parameters: this script is INTERACTIVE (it asks for input) but AAA_QUIET=1 is set (a silent run, nobody can answer); run it in your own terminal without AAA_QUIET"
# bash executes a script piece by piece, so a syntax error near its end would only show after the earlier steps ran.
bash -n "$_AAA_SCRIPT" || _aaa_refuse "VALIDATION FAILED: the script does not parse (bash -n)"
# The controller checked the run itself: its name, its state, its script. In plain AAA the directory's name is the
# run's name, so the name inside must match it, and a finished run never runs again: a rerun is a new run.
if [[ "$_AAA_PLINTH" != 1 ]]; then
  [[ "$(basename "$_AAA_SCRIPT")" == ask.sh && "$(basename "$_AAA_DIR")" == "$RUN_NAME" ]] || _aaa_refuse "VALIDATION FAILED: RUN_NAME does not match the run directory $(basename "$_AAA_DIR")/$(basename "$_AAA_SCRIPT")"
  compgen -G "$_AAA_DIR/answer*" > /dev/null && _aaa_refuse "this run has finished already: a rerun is a new run (plinthctl new-ask)"
fi
cd "$_AAA_DIR" || _aaa_refuse "cd $_AAA_DIR failed"
if [[ "$_AAA_PLINTH" == 1 ]]; then
  [[ -f "$_AAA_ANSWER" && ! -L "$_AAA_ANSWER" && ! -s "$_AAA_ANSWER" ]] || _aaa_refuse "running.yml is not the empty file the controller writes for the start"
else
  ( set -C; : > "$_AAA_ANSWER" ) 2> /dev/null || _aaa_refuse "running.yml exists: the run is running, or was killed (a rerun is a new run: plinthctl new-ask)"
  chmod 600 "$_AAA_ANSWER"
fi

# AAA_QUIET=1: nothing the script prints reaches the caller, so the answer's contents are seen only after the DLP scan.
# fd 3 and 4 are the original stderr and stdout, for fixed lines without content only.
exec 3>&2 4>&1
if [[ "${AAA_QUIET:-}" == 1 ]]; then exec > /dev/null 2> /dev/null; fi

# The document's head, and a first section without a title for anything said before the first section (finish drops
# it when it stays empty).
printf 'apiVersion: plinth/v1\nkind: Answer\nmetadata:\n  run: "%s"\n  started: "%s"\nsections:\n  - title: |2\n    output: |2\n' \
  "$RUN_NAME" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$_AAA_ANSWER"

# _aaa_text [-echo] -indent N: stdin into the answer as lines of a literal block (plinthctl escapes what YAML can't
# hold); -echo also copies it, as it is, to stdout.
_aaa_text() { "$_AAA_BIN" __aaa-text "$@" "$_AAA_ANSWER"; }

# section "<title>": starts a section; say and block write into it.
section() {
  [[ -n "$*" ]] && printf '\n## %s\n' "$*"
  printf '  - title: |2\n' >> "$_AAA_ANSWER"
  printf '%s\n' "$*" | _aaa_text -indent 6
  printf '    output: |2\n' >> "$_AAA_ANSWER"
}
# say <text>: one line into the section (and the terminal).
say() { printf '%s\n' "$*" | _aaa_text -echo -indent 6; }
# ... | block: what comes on stdin into the section (and the terminal).
block() { _aaa_text -echo -indent 6; }
# fail "<why>": stops the run here, as failed (answer.FAIL); the EXIT trap writes why. In the script itself only: in a
# pipeline or $(...) it would end that subshell alone.
fail() { _AAA_FAILURE="${*:-failed}"; exit 1; }
# title <text>: the terminal tab's title, never into the answer.
title() { ( printf '\033]0;%s\007' "$*" > /dev/tty ) 2> /dev/null || true; }
# ask_yes "<question>": the only way a script asks the user something; true only if they type yes. Needs INTERACTIVE=1
# in the header. Deliberately no environment override (no CONFIRM=yes): an agent must not be able to answer for the user.
ask_yes() {
  [[ "$INTERACTIVE" == 1 ]] || fail "ask_yes used but INTERACTIVE=0 in the script's header"
  local a=""
  printf '%s (type yes): ' "$1" > /dev/tty 2> /dev/null
  read -r a < /dev/tty 2> /dev/null || true
  [[ "$a" == yes ]]
}
ssh_q() { local u="$1" h="$2"; shift 2; ssh -o BatchMode=yes -o ConnectTimeout=10 -o LogLevel=ERROR "$u@$h" "$@" 2>&1; }
# ... | scrub: masks secret VALUES (key=value, quoted values, Ceph keys, JWT, bearer, URL passwords, PEM keys); never
# drops a line, so errors stay visible.
scrub() { sed -E -e '/-----BEGIN [A-Z ]*PRIVATE KEY-----/,/-----END [A-Z ]*PRIVATE KEY-----/c\<private key block masked>' -e 's/AQ[A-Za-z0-9+\/]{30,}={0,2}/<ceph-key>/g' -e 's/eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}/<jwt>/g' -e 's/((bearer|basic)[[:space:]]+)[A-Za-z0-9._~+\/=-]{12,}/\1<masked>/Ig' -e 's/(:\/\/[^\/:@[:space:]]+:)[^@\/[:space:]]+@/\1<masked>@/g' -e 's/([A-Za-z0-9_.-]*(passw(or)?d|passphrase|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|auth[_-]?key|client[_-]?key|credentials?)[\x22\x27]?[[:space:]]*[:=][[:space:]]*)(\x22[^\x22]*\x22|\x27[^\x27]*\x27|[^\x22\x27[:space:],;}(][^[:space:],;}]*)/\1<masked>/Ig' | cut -c1-300; }

# The end of the run, however it ends (but a kill -9): the failure, after fail or a status other than 0, and the
# result last, then running.yml becomes answer[.FAIL].new.yml for plinthctl finish (or the controller's scan). A
# running.yml the script removed (or replaced by a symlink) is no answer, and so, under the controller, is a run that
# HUP, INT or TERM ended.
_aaa_end() {
  local st=$? res=ok name=answer.new.yml
  # A script that removed running.yml ends without an answer: nothing here writes a new one.
  if [[ ! -f "$_AAA_ANSWER" || -L "$_AAA_ANSWER" ]]; then
    echo "$RUN_NAME: running.yml is gone - no answer; a rerun is a new run" >&3
    return
  fi
  # Under the controller a run a signal ended (the controller's stop or timeout) has no answer: it may run again,
  # and the controller keeps what running.yml holds when it does.
  if [[ -n "$_AAA_SIGNAL" ]]; then
    echo "$RUN_NAME: ended by SIG$_AAA_SIGNAL - no answer; it may run again" >&3
    return
  fi
  if [[ -z "$_AAA_FAILURE" && "$st" != 0 ]]; then _AAA_FAILURE="the script exited with status $st, not through fail"; fi
  if [[ -n "$_AAA_FAILURE" ]]; then
    res=fail name=answer.FAIL.new.yml
    printf 'failure: |2\n' >> "$_AAA_ANSWER"
    printf '%s\n' "$_AAA_FAILURE" | _aaa_text -indent 2
    printf '\nFAIL: %s - stopped here.\n' "$_AAA_FAILURE"
  fi
  printf 'result: %s\n' "$res" >> "$_AAA_ANSWER"
  mv "$_AAA_ANSWER" "$_AAA_DIR/$name"
  title "$RUN_NAME done"
  if [[ "$_AAA_PLINTH" == 1 ]]; then
    echo "$RUN_NAME done ($res) - next: the controller scans the answer; the user releases it (Done), then the agent reads it" >&4
  else
    echo "$RUN_NAME done ($res) - next: the user runs plinthctl finish $RUN_NAME, then the agent plinthctl read-answer $RUN_NAME" >&4
  fi
}
trap _aaa_end EXIT
# Under the controller HUP, INT and TERM end the run without an answer. bash runs the handler once the command in
# hand has ended; one that outlasts the controller's grace is killed (SIGKILL), which leaves running.yml as it is too.
if [[ "$_AAA_PLINTH" == 1 ]]; then
  trap '_AAA_SIGNAL=HUP; exit 129' HUP
  trap '_AAA_SIGNAL=INT; exit 130' INT
  trap '_AAA_SIGNAL=TERM; exit 143' TERM
fi
title "$RUN_NAME: running"
AAA_LOADED=1
