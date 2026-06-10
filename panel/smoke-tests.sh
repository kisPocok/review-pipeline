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
printf '## Low\n\n### F1: nit\n- **File:** main.go:1\n- **Description:** x\n- **Suggested fix:** y\n' > "$JDIR/final.md"
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
printf '%s\n' '{"type":"result","subtype":"success","result":"## Low\n\n### F1: nit"}'
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
while [[ ! -f "$JOB_DIR/exit_code" && $SECONDS -lt $deadline ]]; do :; done
if [[ -f "$JOB_DIR/exit_code" && -s "$JOB_DIR/final.md" ]]; then
  ok "claude-job: final.md complete when exit_code appears"
else
  fail "claude-job: exit_code appeared before final.md (race), or job never finished"
fi

rm -rf "$WORK"
echo "════════════════════════════"
printf 'summary: %d pass, %d fail\n' "$PASS" "$FAIL"
exit "$FAIL"
