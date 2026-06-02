---
name: review-pipeline
description: How to respond when the review-pipeline pre-commit hook blocks a `git commit`. Fires a 6-lens panel (3 Codex high + 3 Opus xhigh) in parallel, runs a Sonnet xhigh deduper to merge findings into structured JSON, sober-second-opinions the false-positive labels, applies fixes in-session (no fixer subprocess), re-stages, then loops up to MAX_ROUNDS=3 until the panel is clean and the marker is written for the post-fix tree.
---

# review-pipeline

A multi-lens commit-time review. The Go hook at `~/.claude/skills/review-pipeline/hook/bin/pre-commit-check` is registered as a `PreToolUse` on `Bash` and **blocks every real `git commit`** until a marker file exists at `~/.orchestra/markers/<git-write-tree>`. Your job when blocked: classify the diff, run the review pipeline if non-trivial, fix what's valid, write the marker for the final post-fix tree, retry the commit.

## How this skill is triggered

The hook decides; you don't. When you run `git commit -m "..."` and the hook prints stderr containing **`STOP. Invoke the \`review-pipeline\` skill BEFORE this commit.`**, follow this playbook. The hook also prints:
- `effective cwd`
- `git globals` (the `--git-dir` / `--work-tree` form to use when computing write-tree)
- `staged tree` (the hash the marker must match)

Use those exact values. Don't recompute or guess.

This skill is also triggered directly when the user says "review this", "send to panel", "run the review pipeline", or similar — outside a commit attempt. In that case, run the non-trivial path without the marker dance.

## Step 0 — Pre-flight permission warm-up

**Run this before anything else.** Every permission prompt the user will face during the run fires here, upfront, so the async pipeline never stalls waiting for approval.

```bash
# Touch every command pattern used later in the pipeline.
install -d -m 700 \
  "$HOME/.orchestra/markers" \
  "$HOME/.orchestra/panels" \
  "$HOME/.orchestra/jobs/claude" \
  "$HOME/.orchestra/jobs/codex"
touch /tmp/.review-pipeline-preflight && rm /tmp/.review-pipeline-preflight
ls "$HOME/.claude/skills/review-pipeline/panel/review-panel" \
   "$HOME/.claude/skills/review-pipeline/jobs/claude-job" \
   "$HOME/.claude/skills/review-pipeline/jobs/codex-job" >/dev/null
fswatch --version >/dev/null
jq --version >/dev/null
echo "pre-flight OK"
```

If any line fails, stop and fix it (missing tool, wrong path, bad perms) before continuing. Do not proceed with a partial pre-flight.

## Step 1 — Classify the staged diff

Look at `git diff --cached` (or the appropriate scope). Classify:

- **Trivial.** Typo. Single-line cosmetic. Doc-only edit (`.md`, `.txt`, comments). Mechanical rename of a single symbol with no behavior change. Version bump in a manifest. Lockfile regeneration. No semantic logic change of any kind.
- **Non-trivial.** Everything else. When in doubt, treat as non-trivial. Multi-line edits, control-flow changes, new functions, schema/API changes, and behavior changes are NEVER trivial regardless of line count.

## Step 2A — Trivial path: marker-only bypass

Briefly note to the user (one line) that you skipped the panel because the diff was trivial, so the skip is auditable. Then write the marker:

```bash
MDIR="$HOME/.orchestra/markers"
install -d -m 700 "$MDIR"
touch "$MDIR/$(git [globals as printed by the hook] -C <effective_cwd> write-tree)"
git commit ...   # retry; hook consumes marker, commit proceeds
```

Use the exact `effective cwd` and `git globals` the hook printed. Don't guess.

## Step 2B — Non-trivial path: full panel + dedupe + fix loop

### 2B.1 — Write the shared review packet

Before firing the panel, write a one-time context packet to a temp file. The panel script prepends this to every lens's prompt, so the six reviewers see the same framing.

Mandatory sections (write `None — <one-sentence reason>` if a section is genuinely empty; never omit a heading):

```markdown
## Context
<what the change does, in 2–4 sentences>

## Design decisions
- <alternative considered → choice made → reason>
- (or `None — pure mechanical change`)

## Key changes
1. <change 1>
2. <change 2>

## Constraints
- <compatibility / performance / security / deployment / privacy>
- (or `None — standalone change`)

## Specific concerns to challenge
- <concern 1, framed as a question for the reviewers>
- Logic errors, missing edge cases, hidden assumptions
- Architectural soundness; simpler alternatives

## Out of scope
- <things the panel shouldn't flag — known lint warnings, WIP fixtures, follow-up TODOs>
- (or `None — flag anything you see`)
```

Save to `/tmp/review-pipeline-packet-<slug>.md`. **Never inline this as a heredoc** — always pass via `--packet <path>`.

### 2B.2 — Fire the panel

```bash
PANEL_ID=$(~/.claude/skills/review-pipeline/panel/review-panel \
  --repo-root <effective_cwd> \
  --scope staged \
  --packet /tmp/review-pipeline-packet-<slug>.md \
  --name <short-slug>)
```

`review-panel` fires all 6 lenses in parallel and prints the panel-id. The manifest lives at `~/.orchestra/panels/$PANEL_ID/manifest.json` and lists which job-id each lens went to.

### 2B.3 — Wait for the panel

**Require `fswatch`** (`brew install fswatch` if missing). Use event-driven waiting — never a sleep loop.

```bash
PANEL_DIR=~/.orchestra/panels/$PANEL_ID

# Helper: count lens jobs that have written exit_code.
count_done() {
  jq -r '.lenses | to_entries[] | .value.runner + "/" + .value.job_id' "$PANEL_DIR/manifest.json" \
    | while read -r jpath; do
        [ -f "$HOME/.orchestra/jobs/$jpath/exit_code" ] && echo x
      done \
    | wc -l
}

# Build the list of job dirs for this panel (scoped to avoid watching other panels).
# Note: mapfile requires bash 4+; this while-read form works on macOS bash 3.2.
JOB_DIRS=()
while IFS= read -r dir; do
  JOB_DIRS+=("$dir")
done < <(
  jq -r --arg h "$HOME" \
    '.lenses | to_entries[] | $h + "/.orchestra/jobs/" + .value.runner + "/" + .value.job_id' \
    "$PANEL_DIR/manifest.json"
)

# Pre-check: jobs may finish between panel launch and watch start.
DONE=$(count_done)
if [ "$DONE" -lt 6 ]; then
  # Filter order matters: --exclude first, then --include overrides it.
  # No --event filter: fswatch may emit Renamed (not Created) for atomically-written files.
  fswatch -r --exclude '.*' --include '/exit_code$' "${JOB_DIRS[@]}" \
    | while read -r _event; do
        DONE=$(count_done)
        [ "$DONE" -ge 6 ] && exit 0
      done
fi
```

**Why pre-check + re-count on each event?** Pre-check handles the race where all jobs finish before `fswatch` starts. Re-counting on each event (rather than tracking N events) handles the race where some jobs finish after pre-check but before the watch is established.

**If you have parallel work to do** (drafting the PR description, planning the next step), start that work after firing the panel. You'll be notified automatically when the background task completes — do NOT fire `ScheduleWakeup` for this.

### 2B.4 — Validate each lens's `final.md`

For each lens job, check:
- `exit_code` is `0`. Non-zero → treat as failure: surface to the user, do NOT write the marker.
- `final.md` exists and size > 0.
- `final.md` contains either a severity header (`## Critical`, `## High`, `## Medium`, `## Low`) OR the literal "No findings." sentinel.

Any lens that fails validation: STOP. Surface the failure to the user with `stderr.log` tail. Never proceed to dedupe with an incomplete panel.

### 2B.5 — Run the deduper

Concatenate the 6 `final.md` files with clear lens-name headers between them, then fire one Sonnet xhigh job with structured JSON output:

```bash
# Build the deduper input file.
INPUT=$(mktemp /tmp/dedupe-input.XXXXXX.md)
[ -f "$INPUT" ] || { echo "mktemp failed — cannot create deduper input file"; exit 1; }
for lens in security architecture quality security_xcheck frontend test_effectiveness; do
  job_id=$(jq -r ".lenses.$lens.job_id" "$PANEL_DIR/manifest.json")
  runner=$(jq -r ".lenses.$lens.runner" "$PANEL_DIR/manifest.json")
  echo "## Lens report: $lens" >> "$INPUT"
  echo >> "$INPUT"
  cat "$HOME/.orchestra/jobs/$runner/$job_id/final.md" >> "$INPUT"
  echo >> "$INPUT"
done

# Compose the deduper prompt: instructions + input.
PROMPT=$(mktemp /tmp/dedupe-prompt.XXXXXX.md)
[ -f "$PROMPT" ] || { echo "mktemp failed — cannot create deduper prompt file"; exit 1; }
cat ~/.claude/skills/review-pipeline/triage/deduper-prompt.md "$INPUT" > "$PROMPT"

# Fire deduper as a claude-job (research mode — no edits).
# --tier standard maps to Sonnet; --effort xhigh overrides the default high → Sonnet xhigh.
DEDUPE_JOB=$(~/.claude/skills/review-pipeline/jobs/claude-job \
  --tier standard \
  --effort xhigh \
  --mode research \
  --name "${PANEL_ID}-dedupe" \
  --repo-root <effective_cwd> \
  --prompt-file "$PROMPT" \
  --json-schema ~/.claude/skills/review-pipeline/triage/deduper-schema.json)

# Wait for it.
until [ -f "$HOME/.orchestra/jobs/claude/$DEDUPE_JOB/exit_code" ]; do sleep 10; done

# Read findings.json from the deduper's final.md (it should be pure JSON).
FINDINGS_JSON="$HOME/.orchestra/jobs/claude/$DEDUPE_JOB/final.md"
```

> **mktemp guard:** Always validate `$INPUT` and `$PROMPT` exist after `mktemp`. A silent failure (permissions, disk full) produces an empty input file that makes the deduper output garbage with exit 0 — the hardest failure to diagnose.

### 2B.6 — Verify false-positive labels (sober second opinion)

Read every `verdict: false_positive` entry in `findings.json`. For each, look at `verdict_reason`:

- **Strong reason** (cites specific code that contradicts the finding): keep `false_positive`.
- **Weak reason** ("probably fine", "could be intentional", "may be a known pattern", or anything not code-grounded): **promote back to `valid`**. The deduper is Sonnet, not Opus — the safety net is you.

For each promotion, record the override: lens + finding title + the weak reason + your decision. Surface the override list to the user at the end of the loop alongside the fixes.

### 2B.7 — Decide

- If the count of `valid` findings (after your verification pass) is **0** → write the marker for the current staged tree (which is the pre-fix tree). **DONE.** Retry the commit; the hook will consume the marker.
- Else → fix.

### 2B.8 — Apply fixes (in-session)

You apply the fixes yourself, using your own Edit/Write/Grep/Bash tools. No fixer subprocess.

**Fixer discipline (mandatory):**

> Apply exactly the changes the `verdict: valid` findings call for. Do not refactor unrelated code; do not improve naming/comments unless a finding cites it; do not add tests unless a finding requests them. After each fix, briefly note what changed (1 line) in your response to the user. Run typecheck / tests after each fix when the project supports it (look at the project's CLAUDE.md or `package.json` / `go.mod` for the command); if a fix breaks something, adjust and continue. If a finding cannot be addressed within the original change's scope (requires a redesign, a separate migration, out-of-scope test infrastructure, etc.) — defer it unilaterally. List it in your response with a one-line reason, then proceed to write the marker and commit. Do NOT use `AskUserQuestion` to ask for permission to defer. "Surface as deferred" means note it in your response, not block on a question.

Process findings in severity order: critical → high → medium → low. Within a severity, file-group them to avoid multiple seeks to the same file.

### 2B.9 — Re-stage and loop

```bash
git -C <effective_cwd> add -u
```

The tree hash has now changed. The pre-fix marker (if any) is irrelevant. Round counter += 1.

- **If round < MAX_ROUNDS (3)** → go back to step 2B.2 (fire panel for the new tree).
- **If round == MAX_ROUNDS** → STOP. Surface to the user:
  - The findings remaining (with reasons each fix could not be applied).
  - The overrides you made on FP labels in each round.
  - Ask the user what to do: accept remaining findings and ship (they write the marker manually), defer to a follow-up PR, or continue past MAX_ROUNDS (explicit override).

## Async handling rules

- **Always validate `final.md` not just `exit_code`.** Exit 0 with an empty `final.md` is a silent failure, not "no findings."
- **Read job dirs by job-id, not "the latest dir".** Multiple panels can be running concurrently in the same session if you triggered them.
- **Never write the marker on a partial panel.** All 6 lens jobs must have exit_code == 0 with valid `final.md` before any marker is touched.
- **Never write a pre-fix marker for a post-fix tree (or vice versa).** Recompute `git write-tree` immediately before `touch`-ing the marker.

## Defaults

| Parameter   | Value             | Rationale |
|---|---|---|
| Lens count  | always 6          | The panel is the whole point of v2 — don't drop lenses heuristically |
| Lens tier   | `strongest`       | Opus xhigh + Codex high (local codex caps at `high`) — anything weaker defeats the purpose |
| Deduper tier| Sonnet + xhigh (`--tier standard --effort xhigh`) | Judgment is your job; deduper does structural merging |
| MAX_ROUNDS  | 3                 | Hard ceiling; surface to user past this |
| Trivial bypass | always classify first | Doc fixes don't deserve a $5 panel |

## Anti-patterns

- ❌ **Generic packet.** "Just review this diff" → generic results. Fill in every section of the packet template.
- ❌ **Fire and forget.** Always come back for the panel result before committing.
- ❌ **Ignoring low/medium findings to save time.** If they're valid (after your FP verification), fix them.
- ❌ **Blindly applying the deduper's FP labels.** It's Sonnet, not Opus. Verify each rejection's reason has code citations.
- ❌ **Skipping FP verification because "the panel seems clean."** If the deduper reports `rejected_count > 0`, you read every rejection.
- ❌ **Treating a non-zero `exit_code` as "no findings — looks clean."** Inspect `stderr.log` first.
- ❌ **Writing the marker on partial-panel completion.** All 6 lenses must finish cleanly.
- ❌ **Reusing a pre-fix tree hash as a post-fix marker.** Recompute `git write-tree` after `git add -u`.
- ❌ **Inlining the packet or deduper prompt as a heredoc.** Always write to a file and pass via `--prompt-file` / `--packet`.
- ❌ **Calling a non-trivial diff "trivial" to take the bypass.** The classification rule is "no semantic logic change." Multi-line edits, control-flow changes, new functions, schema/API changes, behavior changes are NEVER trivial regardless of line count.
- ❌ **Bypassing the hook by wrapping the commit in a shell form the parser doesn't reach** (heredoc-to-bash, deeply nested subshells, `eval`). If you find yourself reaching for a wrapper to dodge the block, that's the signal to actually run the panel.
- ❌ **Skipping classification in autonomous mode.** Trivial diffs still get the bypass; the autonomous distinction only changes whether you ASK the user about non-trivial panels — it doesn't promote trivial diffs into reviewable ones.
- ❌ **Letting fixer scope creep happen.** Each fix is scoped to its finding. Adjacent "improvements" are out of scope and create new findings the next round will catch, looping forever.
- ❌ **Hiding overrides from the user.** If you promoted any false_positive back to valid, the user must see that in your final response.
- ❌ **Cheating MAX_ROUNDS.** Three rounds is the cap. If you can't converge, surface to the user — don't keep firing panels.
- ❌ **Chaining `git add` and `git commit` in the same Bash call.** The hook fires as `PreToolUse` and blocks the entire command — `git add` never runs either. Always issue them as separate Bash tool calls: stage first, confirm with `git diff --cached --stat`, then commit.
- ❌ **Using `ScheduleWakeup` to wait for panel or deduper jobs.** These are background tasks — the harness notifies you automatically when they complete. `ScheduleWakeup` is for `/loop dynamic` mode only. Never fire it inside a review-pipeline run.
