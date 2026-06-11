#!/usr/bin/env bash
# Smoke tests for the two-reviewer review-panel + wait-panel.
#
# Stubs the codex-job / claude-job binaries (via REVIEW_PIPELINE_{CODEX,CLAUDE}_JOB)
# so no real CLI runs, points HOME at a scratch dir, and asserts the manifest +
# prompt shape and wait-panel validation. Exit code = number of failures.

set -u
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REVIEW_PANEL="$SCRIPT_DIR/review-panel"
WAIT_PANEL="$SCRIPT_DIR/wait-panel"
PASS=0; FAIL=0

fail() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
ok()   { PASS=$((PASS+1)); printf '  ok    %s\n' "$1"; }

WORK=$(mktemp -d /tmp/review-panel-smoke.XXXXXX)
export HOME="$WORK/home"
mkdir -p "$HOME"

# Scratch git repo with a staged change.
REPO="$WORK/repo"
mkdir -p "$REPO"
git -C "$REPO" init -q
git -C "$REPO" config user.email t@t.t
git -C "$REPO" config user.name t
printf 'package main\nfunc main(){}\n' > "$REPO/main.go"
git -C "$REPO" add main.go

# Shared stub body: writes a fake job dir with a valid final.md and echoes the id.
# $1 is the runner name (codex|claude); the rest are the job flags.
STUB="$WORK/stub-job"
cat > "$STUB" <<'STUBEOF'
#!/usr/bin/env bash
set -euo pipefail
RUNNER="$1"; shift
NAME="job"; PROMPT=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --name) NAME="$2"; shift 2 ;;
    --prompt-file) PROMPT="$2"; shift 2 ;;
    *) shift ;;
  esac
done
JID="${RUNNER}-${NAME}-stub-$$-${RANDOM}"
JDIR="$HOME/.orchestra/jobs/$RUNNER/$JID"
mkdir -p "$JDIR"
cp "$PROMPT" "$JDIR/prompt.txt"
printf '## Trace log\n- concern -> main.go:1 -> checked\n\n## Low\n\n### F1: nit\n- **File:** main.go:1\n- **Description:** x\n- **Suggested fix:** y\n' > "$JDIR/final.md"
echo 0 > "$JDIR/exit_code"
echo "$JID"
STUBEOF
chmod +x "$STUB"

# One wrapper per runner: each prepends its runner name as $1 to the shared stub.
for r in codex claude; do
  printf '#!/usr/bin/env bash\nexec "%s" %s "$@"\n' "$STUB" "$r" > "$WORK/stub-$r"
  chmod +x "$WORK/stub-$r"
done

PACKET="$WORK/packet.md"
printf '## Context\nTest change.\n' > "$PACKET"

PANEL_ID=$(
  REVIEW_PIPELINE_CODEX_JOB="$WORK/stub-codex" \
  REVIEW_PIPELINE_CLAUDE_JOB="$WORK/stub-claude" \
  "$REVIEW_PANEL" --repo-root "$REPO" --scope staged --packet "$PACKET" --name smoke 2>/dev/null
)

MANIFEST="$HOME/.orchestra/panels/$PANEL_ID/manifest.json"
PROMPT_FILE="$HOME/.orchestra/panels/$PANEL_ID/prompt.md"

# Assertion 1: manifest exists and has exactly two reviewers.
n=$(jq -r '.reviewers | length' "$MANIFEST" 2>/dev/null)
[[ "$n" == "2" ]] && ok "manifest has 2 reviewers" || fail "manifest reviewers=$n (want 2)"

# Assertion 2: both reviewer job_ids present.
jq -e '.reviewers.claude.job_id and .reviewers.codex.job_id' "$MANIFEST" >/dev/null 2>&1 \
  && ok "claude + codex job_ids present" || fail "missing claude/codex job_id"

# Assertion 3: the shared prompt contains the preamble concerns + the diff.
grep -qi 'correctness' "$PROMPT_FILE" && ok "prompt carries preamble (correctness)" \
  || fail "prompt missing preamble"
grep -q 'main.go' "$PROMPT_FILE" && ok "prompt carries the diff" \
  || fail "prompt missing diff"

# Assertion 4: wait-panel validates both reviewers and exits 0.
if OUT=$("$WAIT_PANEL" "$PANEL_ID" 2>/dev/null); then
  ok "wait-panel exit 0 (both reviewers valid)"
else
  fail "wait-panel non-zero on valid panel"
fi

# Assertion 5: on success wait-panel prints both reviewers' final.md paths —
# the contract SKILL.md 2B.5 depends on to avoid the simple_expansion prompt.
if grep -q '^  claude .*/final\.md$' <<<"$OUT" && grep -q '^  codex .*/final\.md$' <<<"$OUT"; then
  ok "wait-panel prints both final.md paths on success"
else
  fail "wait-panel missing reviews-ready paths block"
fi

# Assertion 6: manifest records the tree the panel reviewed (the pre-fix tree
# at fire time — the next round's baseline).
EXPECTED_TREE=$(git -C "$REPO" write-tree)
REVIEWED_TREE=$(jq -r '.reviewed_tree // empty' "$MANIFEST" 2>/dev/null)
if [[ "$REVIEWED_TREE" == "$EXPECTED_TREE" ]]; then
  ok "manifest records reviewed_tree"
else
  fail "manifest reviewed_tree='$REVIEWED_TREE' (want $EXPECTED_TREE)"
fi

# Assertion 7: on success wait-panel prints the literal next-round baseline —
# SKILL.md 2B.3 copies this into round N+1's --scope, no write-tree capture.
if grep -q "^next baseline: tree:$EXPECTED_TREE$" <<<"$OUT"; then
  ok "wait-panel prints next baseline line"
else
  fail "wait-panel missing 'next baseline: tree:<hash>' line"
fi

# Assertion 25-27: wait-panel requires a '## Trace log' header in final.md.
make_trace_panel() {  # $1=panel-id  $2=claude-final-content
  local pid="$1" body="$2"
  local pdir="$HOME/.orchestra/panels/$pid"
  mkdir -p "$pdir"
  for r in claude codex; do
    local jid="${r}-${pid}-job"
    local jdir="$HOME/.orchestra/jobs/$r/$jid"
    mkdir -p "$jdir"
    if [[ "$r" == "claude" ]]; then
      printf '%b' "$body" > "$jdir/final.md"
    else
      printf '## Trace log\n- concern -> main.go:1 -> checked\n\nNo findings. Checked the diff.\n' > "$jdir/final.md"
    fi
    echo 0 > "$jdir/exit_code"
  done
  jq -n --arg pid "$pid" '{
    panel_id: $pid,
    reviewers: {
      claude: {runner: "claude", job_id: ("claude-" + $pid + "-job")},
      codex:  {runner: "codex",  job_id: ("codex-"  + $pid + "-job")}
    }
  }' > "$pdir/manifest.json"
}

# 25: findings but no trace log — must FAIL validation (exit 1).
make_trace_panel tracelog-missing '## Low\n\n### F1: nit\n- **File:** main.go:1\n- **Description:** x\n- **Suggested fix:** y\n'
if "$WAIT_PANEL" tracelog-missing >/dev/null 2>&1; then
  fail "wait-panel accepted final.md without a trace log"
else
  ok "wait-panel rejects final.md missing trace log"
fi

# 26: findings plus trace log — must pass (exit 0).
make_trace_panel tracelog-findings '## Trace log\n- concern -> main.go:1 -> checked\n\n## Low\n\n### F1: nit\n- **File:** main.go:1\n- **Description:** x\n- **Suggested fix:** y\n'
if "$WAIT_PANEL" tracelog-findings >/dev/null 2>&1; then
  ok "wait-panel accepts findings + trace log"
else
  fail "wait-panel rejected a valid final.md with trace log"
fi

# 27: 'No findings.' plus trace log — must pass (exit 0).
make_trace_panel tracelog-nofindings '## Trace log\n- concern -> main.go:1 -> checked\n\nNo findings. Checked the diff.\n'
if "$WAIT_PANEL" tracelog-nofindings >/dev/null 2>&1; then
  ok "wait-panel accepts 'No findings.' + trace log"
else
  fail "wait-panel rejected 'No findings.' with trace log"
fi

# 28: trace log buried after the findings — must FAIL (the protocol
# mandates it before any severity header; presence alone is not enough).
make_trace_panel tracelog-buried '## Low\n\n### F1: nit\n- **File:** main.go:1\n- **Description:** x\n- **Suggested fix:** y\n\n## Trace log\n- concern -> main.go:1 -> checked\n'
if "$WAIT_PANEL" tracelog-buried >/dev/null 2>&1; then
  fail "wait-panel accepted a trace log placed after findings"
else
  ok "wait-panel rejects trace log after findings"
fi

# ── claude-job ──────────────────────────────────────────────────────────────
CLAUDE_JOB="$SCRIPT_DIR/../jobs/claude-job"
STUBBIN="$WORK/stubbin"
mkdir -p "$STUBBIN"
REAL_JQ=$(command -v jq)

# Stub claude CLI: emits stream-json with narration, findings text, and a
# terminal result event.
cat > "$STUBBIN/claude" <<'EOF'
#!/usr/bin/env bash
cat >/dev/null
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"Reading the diff to get oriented."}]}}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"## Low\n\n### F1: nit"}]}}'
if [[ -z "${CLAUDE_STUB_NO_RESULT:-}" ]]; then
  printf '%s\n' '{"type":"result","subtype":"success","result":"## Low\n\n### F1: nit"}'
fi
EOF
chmod +x "$STUBBIN/claude"

# jq wrapper that delays only the events.jsonl extraction call, making any
# exit_code-before-final.md ordering bug deterministic instead of a ms race.
cat > "$STUBBIN/jq" <<EOF
#!/usr/bin/env bash
for a in "\$@"; do [[ "\$a" == *events.jsonl* ]] && sleep 1; done
exec "$REAL_JQ" "\$@"
EOF
chmod +x "$STUBBIN/jq"

JOB_ID=$(PATH="$STUBBIN:$PATH" "$CLAUDE_JOB" --tier light --mode review \
  --prompt-file "$PACKET" --repo-root "$REPO" --name smoke 2>/dev/null)
JOB_DIR="$HOME/.orchestra/jobs/claude/$JOB_ID"

# Assertion 8: exit_code is the completion sentinel — final.md must already be
# complete the instant exit_code appears (wait-panel polls on exit_code).
deadline=$((SECONDS + 20))
while [[ ! -f "$JOB_DIR/exit_code" && $SECONDS -lt $deadline ]]; do sleep 0.1; done
if [[ -f "$JOB_DIR/exit_code" && -s "$JOB_DIR/final.md" ]]; then
  ok "claude-job: final.md complete when exit_code appears"
else
  fail "claude-job: exit_code appeared before final.md (race), or job never finished"
fi

# Assertion 9: final.md is the terminal result event only — intermediate
# assistant narration must not pollute the review.
if grep -q 'F1: nit' "$JOB_DIR/final.md" && ! grep -q 'Reading the diff' "$JOB_DIR/final.md"; then
  ok "claude-job: final.md carries result event, no narration"
else
  fail "claude-job: final.md polluted with narration or missing result text"
fi

# Assertion 10: with no result event in the stream, fall back to assistant text.
JOB_ID2=$(PATH="$STUBBIN:$PATH" CLAUDE_STUB_NO_RESULT=1 "$CLAUDE_JOB" --tier light --mode review \
  --prompt-file "$PACKET" --repo-root "$REPO" --name smoke-noresult 2>/dev/null)
JOB_DIR2="$HOME/.orchestra/jobs/claude/$JOB_ID2"
deadline=$((SECONDS + 20))
while [[ ! -f "$JOB_DIR2/exit_code" && $SECONDS -lt $deadline ]]; do sleep 0.1; done
if grep -q 'F1: nit' "$JOB_DIR2/final.md" 2>/dev/null; then
  ok "claude-job: falls back to assistant text without result event"
else
  fail "claude-job: empty final.md when stream lacks a result event"
fi

# Assertion 11: wait-panel must not hang forever when a job dies before
# writing exit_code — it deadlines (WAIT_PANEL_TIMEOUT_SECS) and exits non-zero.
PANEL_ID3=$(
  REVIEW_PIPELINE_CODEX_JOB="$WORK/stub-codex" \
  REVIEW_PIPELINE_CLAUDE_JOB="$WORK/stub-claude" \
  "$REVIEW_PANEL" --repo-root "$REPO" --scope staged --packet "$PACKET" --name smoke-dead 2>/dev/null
)
DEAD_JID=$(jq -r '.reviewers.codex.job_id' "$HOME/.orchestra/panels/$PANEL_ID3/manifest.json")
rm -f "$HOME/.orchestra/jobs/codex/$DEAD_JID/exit_code"
TIMEOUT_BIN=$(command -v gtimeout || command -v timeout || true)
if [[ -z "$TIMEOUT_BIN" ]]; then
  fail "no gtimeout/timeout on PATH — cannot run dead-job deadline test"
else
  WAIT_PANEL_TIMEOUT_SECS=2 "$TIMEOUT_BIN" 15 "$WAIT_PANEL" "$PANEL_ID3" >/dev/null 2>&1
  rc=$?
  if [[ "$rc" == "1" ]]; then
    ok "wait-panel deadlines and exits non-zero on a dead job"
  elif [[ "$rc" == "124" ]]; then
    fail "wait-panel hung past its deadline (killed by test timeout)"
  else
    fail "wait-panel exit $rc on a dead job (want 1)"
  fi
fi

# Assertion 19: a failed codex-job launch must not orphan the already-fired
# claude job — review-panel exits non-zero and terminates the claude process.
cat > "$WORK/stub-claude-orphan" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
NAME="job"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --name) NAME="$2"; shift 2 ;;
    *) shift ;;
  esac
done
JID="claude-${NAME}-stub-$$-${RANDOM}"
JDIR="$HOME/.orchestra/jobs/claude/$JID"
mkdir -p "$JDIR"
sleep 60 &
echo $! > "$JDIR/pid"
echo "$JID"
EOF
printf '#!/usr/bin/env bash\necho "codex-job: boom" >&2\nexit 1\n' > "$WORK/stub-codex-fail"
chmod +x "$WORK/stub-claude-orphan" "$WORK/stub-codex-fail"

if REVIEW_PIPELINE_CODEX_JOB="$WORK/stub-codex-fail" \
   REVIEW_PIPELINE_CLAUDE_JOB="$WORK/stub-claude-orphan" \
   "$REVIEW_PANEL" --repo-root "$REPO" --scope staged --packet "$PACKET" \
   --name smoke-orphan >/dev/null 2>"$WORK/orphan.err"; then
  fail "review-panel exited 0 despite codex-job launch failure"
else
  ok "review-panel exits non-zero when codex-job launch fails"
fi
if grep -q 'cancelling claude job' "$WORK/orphan.err"; then
  ok "review-panel reports the cancelled claude job"
else
  fail "review-panel stderr missing cancellation notice: $(cat "$WORK/orphan.err")"
fi
ORPHAN_PID=$(cat "$HOME"/.orchestra/jobs/claude/claude-smoke-orphan-*/pid 2>/dev/null || true)
if [[ -n "$ORPHAN_PID" ]] && ! kill -0 "$ORPHAN_PID" 2>/dev/null; then
  ok "claude job process terminated, not orphaned"
else
  fail "claude job process still running (pid=$ORPHAN_PID)"
  kill "$ORPHAN_PID" 2>/dev/null
fi

# ── check-dispositions ──────────────────────────────────────────────────────
CHECK_DISP="$SCRIPT_DIR/../triage/check-dispositions"

# Helper: write a dispositions.json under a fresh panel-id and run the check.
# $1 = panel-id suffix, $2 = dispositions JSON array (as a string).
write_disp() {
  local pid="disp-$1"
  mkdir -p "$HOME/.orchestra/panels/$pid"
  printf '{"panel_id":"%s","round":1,"findings_path":"x","dispositions":%s}\n' \
    "$pid" "$2" > "$HOME/.orchestra/panels/$pid/dispositions.json"
  echo "$pid"
}

# Assertion 12: medium fixed remaining → proceed to fix, exit 0.
PID=$(write_disp proceed '[
  {"finding_id":"F001","severity":"medium","outcome":"fixed","reason":"r"},
  {"finding_id":"F002","severity":"low","outcome":"false_positive","reason":"r"}]')
OUT=$("$CHECK_DISP" "$PID" 2>&1); rc=$?
if [[ $rc -eq 0 ]] && grep -q '^verdict: proceed to fix' <<<"$OUT"; then
  ok "check-dispositions: proceed verdict on fixed medium"
else
  fail "check-dispositions: want proceed verdict + exit 0, got rc=$rc out=$OUT"
fi

# Assertion 13: unfixed high gates, even with other fixes pending.
PID=$(write_disp gate '[
  {"finding_id":"F001","severity":"high","outcome":"acknowledged","reason":"r"},
  {"finding_id":"F002","severity":"medium","outcome":"fixed","reason":"r"}]')
OUT=$("$CHECK_DISP" "$PID" 2>&1); rc=$?
if [[ $rc -eq 0 ]] && grep -q '^verdict: GATE — 1 unfixed critical/high' <<<"$OUT"; then
  ok "check-dispositions: GATE verdict on unfixed high"
else
  fail "check-dispositions: want GATE verdict, got rc=$rc out=$OUT"
fi

# Assertion 14: nothing to fix (only ack/fp, no critical/high among them).
PID=$(write_disp done '[
  {"finding_id":"F001","severity":"low","outcome":"acknowledged","reason":"r"}]')
OUT=$("$CHECK_DISP" "$PID" 2>&1); rc=$?
if [[ $rc -eq 0 ]] && grep -q '^verdict: nothing to fix' <<<"$OUT"; then
  ok "check-dispositions: nothing-to-fix verdict"
else
  fail "check-dispositions: want nothing-to-fix verdict, got rc=$rc out=$OUT"
fi

# Assertion 15: all fixed findings are LOW → LOW-only verdict.
PID=$(write_disp lowonly '[
  {"finding_id":"F001","severity":"low","outcome":"fixed","reason":"r"},
  {"finding_id":"F002","severity":"low","outcome":"fixed","reason":"r"}]')
OUT=$("$CHECK_DISP" "$PID" 2>&1); rc=$?
if [[ $rc -eq 0 ]] && grep -q '^verdict: LOW-only' <<<"$OUT"; then
  ok "check-dispositions: LOW-only verdict"
else
  fail "check-dispositions: want LOW-only verdict, got rc=$rc out=$OUT"
fi

# Assertion 16: empty dispositions array is legitimate (both reviewers clean).
PID=$(write_disp empty '[]')
OUT=$("$CHECK_DISP" "$PID" 2>&1); rc=$?
if [[ $rc -eq 0 ]] && grep -q '^verdict: nothing to fix' <<<"$OUT"; then
  ok "check-dispositions: empty array valid, nothing to fix"
else
  fail "check-dispositions: empty array should pass, got rc=$rc out=$OUT"
fi

# Assertion 17: malformed entry (missing severity) → exit 1.
PID=$(write_disp bad '[
  {"finding_id":"F001","outcome":"fixed","reason":"r"}]')
if "$CHECK_DISP" "$PID" >/dev/null 2>&1; then
  fail "check-dispositions: accepted an entry with no severity"
else
  [[ $? -eq 1 ]] && ok "check-dispositions: rejects missing severity with exit 1" \
    || fail "check-dispositions: wrong exit code on invalid entry"
fi

# Assertion 18: missing dispositions.json → exit 2.
"$CHECK_DISP" "no-such-panel-id" >/dev/null 2>&1
[[ $? -eq 2 ]] && ok "check-dispositions: exit 2 on missing file" \
  || fail "check-dispositions: want exit 2 on missing file"

# ── preflight ────────────────────────────────────────────────────────────────
# Copy preflight into a scratch skill tree — it checks paths relative to its
# own location, so the real tree would mask a missing-file regression.
PF_SKILL="$WORK/pf-skill"
mkdir -p "$PF_SKILL/panel" "$PF_SKILL/triage" "$PF_SKILL/jobs" "$PF_SKILL/hook/bin"
cp "$SCRIPT_DIR/preflight" "$PF_SKILL/panel/preflight"
for f in panel/review-panel panel/wait-panel panel/write-marker \
         triage/check-dispositions jobs/claude-job jobs/codex-job; do
  printf '#!/bin/sh\n' > "$PF_SKILL/$f"
  chmod +x "$PF_SKILL/$f"
done
: > "$PF_SKILL/panel/reviewer-preamble.md"
printf '#!/bin/sh\nexit 0\n' > "$STUBBIN/codex"
chmod +x "$STUBBIN/codex"

# Assertion 23: missing hook binary is fatal — it's gitignored, so a fresh
# clone without install.sh would otherwise commit with no review, silently.
ERR=$(PATH="$STUBBIN:$PATH" "$PF_SKILL/panel/preflight" 2>&1 >/dev/null); rc=$?
if [[ $rc -ne 0 ]] && grep -q 'hook binary' <<<"$ERR"; then
  ok "preflight: fails on missing hook binary"
else
  fail "preflight: rc=$rc with no hook binary (want non-zero + 'hook binary'): $ERR"
fi

# Assertion 24: no gtimeout/timeout on PATH → non-fatal warning (reviewer
# jobs run unbounded without one), exit stays 0.
printf '#!/bin/sh\n' > "$PF_SKILL/hook/bin/pre-commit-check"
chmod +x "$PF_SKILL/hook/bin/pre-commit-check"
ONLYBIN="$WORK/onlybin"
mkdir -p "$ONLYBIN"
for t in bash dirname install mktemp rm jq git; do
  ln -s "$(command -v "$t")" "$ONLYBIN/$t"
done
for t in claude codex; do
  printf '#!/bin/sh\nexit 0\n' > "$ONLYBIN/$t"
  chmod +x "$ONLYBIN/$t"
done
ERR=$(PATH="$ONLYBIN" "$PF_SKILL/panel/preflight" 2>&1 >/dev/null); rc=$?
if [[ $rc -eq 0 ]] && grep -q 'gtimeout' <<<"$ERR"; then
  ok "preflight: warns non-fatally when gtimeout/timeout missing"
else
  fail "preflight: rc=$rc err=$ERR (want exit 0 + gtimeout warning)"
fi

# ── install.sh --with-permissions ───────────────────────────────────────────
# Run a copy of install.sh against the scratch HOME with a stubbed `go`, so
# the test never touches the real binary, skills symlink, or settings.
INSTALL_SRC="$WORK/install-src"
mkdir -p "$INSTALL_SRC/hook/cmd/pre-commit-check"
cp "$SCRIPT_DIR/../install.sh" "$INSTALL_SRC/install.sh"

GOSTUB="$WORK/gostub"
mkdir -p "$GOSTUB"
cat > "$GOSTUB/go" <<'EOF'
#!/usr/bin/env bash
out=""
while [[ $# -gt 0 ]]; do
  case "$1" in -o) out="$2"; shift 2 ;; *) shift ;; esac
done
[[ -n "$out" ]] || exit 1
mkdir -p "$(dirname "$out")"
printf '#!/bin/sh\n' > "$out"
chmod +x "$out"
EOF
chmod +x "$GOSTUB/go"

SETTINGS_FILE="$HOME/.claude/settings.json"
rm -f "$SETTINGS_FILE"

# Assertion 20: plain install must not write any permissions.
PATH="$GOSTUB:$PATH" "$INSTALL_SRC/install.sh" >/dev/null 2>&1
if jq -e '.permissions == null' "$SETTINGS_FILE" >/dev/null 2>&1; then
  ok "install.sh: no permissions written without --with-permissions"
else
  fail "install.sh: wrote permissions without the flag"
fi

# Seed a user entry to prove the merge preserves what's already there.
TMPJ=$(mktemp)
jq '.permissions.allow = ["Bash(custom:*)"]' "$SETTINGS_FILE" > "$TMPJ"
mv "$TMPJ" "$SETTINGS_FILE"

# Assertion 21: --with-permissions merges the README allowlist, keeping
# pre-existing user entries.
PATH="$GOSTUB:$PATH" "$INSTALL_SRC/install.sh" --with-permissions >/dev/null 2>&1
if jq -e '
  .permissions.allow
  | index("Bash(custom:*)")
    and index("Bash(~/.claude/skills/review-pipeline/panel/preflight)")
    and index("Bash(~/.claude/skills/review-pipeline/triage/check-dispositions:*)")
    and index("Write(/tmp/review-pipeline-packet-*.md)")
' "$SETTINGS_FILE" >/dev/null 2>&1; then
  ok "install.sh: --with-permissions merges allowlist, keeps user entries"
else
  fail "install.sh: allowlist merge wrong: $(jq -c '.permissions' "$SETTINGS_FILE" 2>/dev/null)"
fi

# Assertion 22: re-running with the flag is idempotent — no duplicates.
N1=$(jq '.permissions.allow | length' "$SETTINGS_FILE" 2>/dev/null)
PATH="$GOSTUB:$PATH" "$INSTALL_SRC/install.sh" --with-permissions >/dev/null 2>&1
N2=$(jq '.permissions.allow | length' "$SETTINGS_FILE" 2>/dev/null)
if [[ -n "$N1" && "$N1" == "$N2" ]]; then
  ok "install.sh: --with-permissions idempotent ($N1 entries)"
else
  fail "install.sh: rerun changed allow count ($N1 -> $N2)"
fi

rm -rf "$WORK"
echo "════════════════════════════"
printf 'summary: %d pass, %d fail\n' "$PASS" "$FAIL"
exit "$FAIL"
