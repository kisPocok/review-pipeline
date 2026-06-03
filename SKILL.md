---
name: review-pipeline
description: Use when the user asks for a code review, or when the pre-commit / PreToolUse hook blocks a `git commit` on this repo. Triggers on phrases like "review my changes", "check this diff", "review the branch", or any time a commit is intercepted and needs to clear before retrying.
---

# review-pipeline

A multi-lens commit-time review. The Go hook at `~/.claude/skills/review-pipeline/hook/bin/pre-commit-check` is registered as a `PreToolUse` on `Bash` and **blocks every real `git commit`** until a marker file exists at `~/.orchestra/markers/<git-write-tree>`. Your job when blocked: classify the diff, run the review pipeline if non-trivial, fix what's valid, write the marker for the final post-fix tree, retry the commit.

## ⛔ FIRST ACTION — Pre-flight permission warm-up (DO NOT SKIP)

🛑 **STOP. This is the first thing you do when this skill is invoked.** Before reading the rest of this file, before looking at `git diff`, before classifying anything — run the block below. It surfaces every permission prompt the pipeline will trigger, upfront, in one shot. **Skip this and the async pipeline stalls mid-run waiting for approval the agent firing it cannot give**, silently — you will not get a useful error, just a hang.

This is not optional setup. This is not "if you have time". This is the first action. Run the command, confirm it prints `pre-flight OK`, then continue to "How this skill is triggered".

```bash
~/.claude/skills/review-pipeline/panel/preflight
```

The script creates the runtime dirs, smoke-tests `/tmp` writability, verifies every pipeline script is present and executable, and confirms `jq`, `git`, `claude`, `codex` are on `PATH`. If it exits non-zero, stop and fix the failure it printed before continuing. Do not proceed with a partial pre-flight.

## Step 1 — Classify the staged diff

Look at `git diff --cached` (or the appropriate scope). Classify:

- **Trivial.** Typo. Single-line cosmetic. Doc-only edit (`.md`, `.txt`, comments). Mechanical rename of a single symbol with no behavior change. Version bump in a manifest. Lockfile regeneration. No semantic logic change of any kind.
- **Non-trivial.** Everything else. When in doubt, treat as non-trivial. Multi-line edits, control-flow changes, new functions, schema/API changes, and behavior changes are NEVER trivial regardless of line count.

## Step 2A — Trivial path: marker-only bypass

Briefly note to the user (one line) that you skipped the panel because the diff was trivial, so the skip is auditable. Then write the marker:

```bash
~/.claude/skills/review-pipeline/panel/write-marker <effective_cwd> [git globals as printed by the hook]
git commit ...   # retry; hook consumes marker, commit proceeds
```

`write-marker` runs `git [globals] -C <cwd> write-tree`, ensures `~/.orchestra/markers/` exists, touches the marker file, and prints `<tree-hash> <marker-path>` on stdout. Use the exact `effective cwd` and `git globals` the hook printed.

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

### 2B.1.5 — Cycle concept (read before firing the panel)

A "cycle" is the sequence of rounds (panel → fix → panel → fix → ...) needed to clear one staged diff. Each round is its own panel-id, but they share a baseline:

- **Round 1** reviews the full diff (`--scope staged`, the default). This is the *original* feature being reviewed.
- **Round 2+** reviews **only the fix delta** since the previous round — `--scope tree:<post-fix-tree-from-round-N-1>`. This prevents already-reviewed code from being re-flagged on every round.

You will need to track in-session, per cycle:

- An array of panel-ids, one per round, in order
- The post-fix tree hash captured at the end of each round (used as the baseline for the next round)
- For each panel-id, the path to its `dispositions.json` (written by you in 2B.7)

Use your TodoList (or scratch notes — the harness allows it) to hold this. There is no persistent cycle state file; the data is derivable from `~/.orchestra/panels/<panel-id>/` for any panel-id you remember.

### 2B.2 — Fire the panel

**Round 1** (the first panel of the cycle — reviews the full staged diff):

```bash
PANEL_ID=$(~/.claude/skills/review-pipeline/panel/review-panel \
  --repo-root <effective_cwd> \
  --scope staged \
  --packet /tmp/review-pipeline-packet-<slug>.md \
  --name <short-slug>-r1)
```

**Round 2+** (reviews only the fix delta against the previous round's post-fix tree):

```bash
PANEL_ID=$(~/.claude/skills/review-pipeline/panel/review-panel \
  --repo-root <effective_cwd> \
  --scope "tree:$PREVIOUS_TREE" \
  --packet /tmp/review-pipeline-packet-<slug>.md \
  --name <short-slug>-r<N>)
```

Where `$PREVIOUS_TREE` is the tree hash captured at the end of the previous round (see 2B.9). The diff the lenses see is `<previous-tree>..<current-write-tree>` — the fixes you applied to address the previous round's findings, plus any incidental staged changes since.

`review-panel` fires all 6 lenses in parallel and prints the panel-id. The manifest lives at `~/.orchestra/panels/$PANEL_ID/manifest.json` and lists which job-id each lens went to. The manifest's `scope` field records the round's review scope (e.g., `tree:abc123…`) for audit.

**Why frozen baseline on round 2+:** if round N re-reviewed the full `--cached` diff, the lenses would re-flag every line of the original feature plus the fixes you applied — including new tests, helper functions, and guards added in response to round-N-1 findings. That's the runaway-iteration dynamic. By scoping to the fix delta, the per-round review surface shrinks instead of growing.

### 2B.3 — Wait for the panel and validate

One command polls for all 6 lens `exit_code` files and validates each `final.md`:

```bash
~/.claude/skills/review-pipeline/panel/wait-panel "$PANEL_ID"
```

Behavior:
- Polls every 2s, logging only on state change (so stderr stays ~7 lines total regardless of total wait time).
- Validates: `exit_code` is `0`, `final.md` non-empty, and contains either a severity header (`## Critical|High|Medium|Low`) or the literal `No findings.` sentinel.
- Exits `0` only if all 6 pass. Non-zero exit → surface the summary table to the user; do **not** proceed to dedupe.

**If you have parallel work to do** (drafting the PR description, planning the next step), start that work after firing the panel. You'll be notified automatically when the background task completes — do NOT fire `ScheduleWakeup` for this.

### 2B.5 — Run the deduper

One command builds the dedupe input from the 6 `final.md`s, fires an Opus xhigh deduper job with the JSON schema, waits for completion, and prints the path to the findings JSON:

```bash
FINDINGS_JSON=$(~/.claude/skills/review-pipeline/panel/run-dedupe "$PANEL_ID")
```

Behavior:
- Writes scratch files to `~/.orchestra/panels/<panel-id>/dedupe/{input.md,prompt.md}` — per-panel dir, no `/tmp` collisions, no `mktemp` template pitfalls.
- Refuses to fire if any lens's `final.md` is missing or empty (silent-failure guard).
- Polls the deduper job's `exit_code` every 2s.
- Exits non-zero on deduper failure or empty output. On success, stdout is the absolute path to the findings JSON (the deduper's `final.md`).

### 2B.6 — Decide a disposition for every finding

For each finding in the deduper's findings JSON (the path is `$FINDINGS_JSON` from 2B.5), decide exactly one outcome:

| Outcome | When to use |
|---|---|
| `fixed` | You will apply a code change this round that addresses the finding. |
| `acknowledged` | Finding is real and valid, but you are accepting the risk in this PR (out-of-scope, deferred to follow-up, spec-mandated, etc.). Must justify in `reason` and ideally cite where the deferral is tracked (e.g., `docs/X/SECURITY-NOTES.md` or a Linear/Jira ticket). |
| `false_positive` | Either the deduper marked it FP and cited specific code that you verified, OR the deduper marked it valid but you verified specific code that contradicts it (in which case set `deduper_override: true`). |

For deduper-marked false-positives, look at `verdict_reason`:

- **Strong reason** (cites specific code that contradicts the finding): record `outcome: false_positive`, copy/paraphrase the deduper's cited code into your `reason`. `deduper_override: false`.
- **Weak reason** ("probably fine", "could be intentional", "may be a known pattern", or anything not code-grounded): **override**. Re-classify as `fixed` or `acknowledged` depending on your decision. Set `deduper_override: true`. Cite your reason explicitly.

For deduper-marked valid findings, the override case (deduper said valid, you verify it doesn't apply) should be rare. When you do it, set `deduper_override: true`, record `outcome: false_positive`, and cite the specific code in `reason`.

**`acknowledged` discipline.** This outcome exists to give you a legitimate exit ramp for findings that are real but not worth fixing in this PR. Use it for: PKCE/HMAC/rate-limiting in a non-security-focused PR; test-tightening on tests that already cover the changed behavior; architectural cleanups that span more files than the PR touches; spec-mandated behavior the lens didn't recognize. Do NOT use it as a synonym for "I don't feel like fixing this" — the reason must be defensible to a reviewer.

### 2B.6.5 — Write the disposition file

Build and write `~/.orchestra/panels/$PANEL_ID/dispositions.json` matching `triage/disposition-schema.json`:

```json
{
  "panel_id": "<panel-id>",
  "round": <1-indexed>,
  "findings_path": "<value of $FINDINGS_JSON from 2B.5 — the deduper's final.md path>",
  "dispositions": [
    {"finding_id": "F001", "outcome": "fixed", "reason": "applied input length cap in <file>:<line>"},
    {"finding_id": "F002", "outcome": "acknowledged", "reason": "PKCE deferred to callback PR; tracked in docs/sso/SECURITY-NOTES.md"},
    {"finding_id": "F003", "outcome": "false_positive", "deduper_override": false, "reason": "deduper cited middleware.Recoverer at routes.go:18; verified."}
  ]
}
```

Write the file before applying fixes (your decisions are made; the file records them). If you change your mind mid-fix (e.g., a `fixed` finding turns out to need a redesign), update the file to `acknowledged` with a reason before refiring the next round.

Validate the file is well-formed JSON:

```bash
jq empty ~/.orchestra/panels/$PANEL_ID/dispositions.json && echo "dispositions OK"
```

### 2B.7 — Decide whether to continue

Count outcomes from the disposition file you just wrote:

- `fixed_count` — findings you will apply code changes for this round.
- `acknowledged_count` — findings you accepted as out-of-scope risk.
- `false_positive_count` — findings you dismissed.

**Terminal cases:**

- If `fixed_count == 0` → no fixes to apply. Write the marker for the current staged tree (`~/.claude/skills/review-pipeline/panel/write-marker <effective_cwd> [git globals]`). **DONE.** Retry the commit; the hook will consume the marker. The dispositions file records what you acknowledged or dismissed.

**Convergence gate (LOW-only):**

- If `fixed_count > 0` but every `outcome: fixed` finding has severity `low` in the findings JSON (i.e., no `critical`/`high`/`medium` remains to fix), STOP and surface to the user. Use `AskUserQuestion` with three options:
  1. **Acknowledge and ship.** Change those LOW findings' dispositions from `fixed` to `acknowledged` (with a `reason` you write per finding), write the marker, retry the commit. (Recommended when the LOWs are test-tightening or comment fixes.)
  2. **Fix and ship.** Apply the LOW fixes this round, but write the marker after this round without refiring another panel. No round N+1.
  3. **Fix and continue.** Apply the LOWs and refire (standard loop). Only choose this if there's a concrete reason to expect new HIGHs hiding behind the LOW fixes.

  Surface the LOW finding titles in the question text so the user can judge.

- Otherwise (HIGH/CRITICAL/MEDIUM remain to fix) → proceed to 2B.8 (apply fixes), then 2B.9 (re-stage and refire).

### 2B.8 — Apply fixes (in-session)

You apply the fixes yourself, using your own Edit/Write/Grep/Bash tools. No fixer subprocess.

**Fixer discipline (mandatory):**

> Apply exactly the changes the `verdict: valid` findings call for. Do not refactor unrelated code; do not improve naming/comments unless a finding cites it; do not add tests unless a finding requests them. After each fix, briefly note what changed (1 line) in your response to the user. Run typecheck / tests after each fix when the project supports it (look at the project's CLAUDE.md or `package.json` / `go.mod` for the command); if a fix breaks something, adjust and continue. If a finding cannot be addressed within the original change's scope (requires a redesign, a separate migration, out-of-scope test infrastructure, etc.) — defer it unilaterally. List it in your response with a one-line reason, then proceed to write the marker and commit. Do NOT use `AskUserQuestion` to ask for permission to defer. "Surface as deferred" means note it in your response, not block on a question.

Process findings in severity order: critical → high → medium → low. Within a severity, file-group them to avoid multiple seeks to the same file.

### 2B.9 — Re-stage, capture post-fix tree, and loop

```bash
git -C <effective_cwd> add -u
POST_FIX_TREE=$(git -C <effective_cwd> write-tree)
echo "round $ROUND post-fix tree: $POST_FIX_TREE"
```

Record `$POST_FIX_TREE` in your in-session cycle state alongside the panel-id. This is the baseline for round N+1's `--scope tree:$POST_FIX_TREE` (per 2B.2).

The pre-fix marker (if any) is irrelevant — the tree has changed. Round counter += 1.

- **If round < MAX_ROUNDS (3)** → go back to step 2B.2 (fire round-N+1 panel with `--scope tree:$POST_FIX_TREE`).
- **If round == MAX_ROUNDS** → STOP. **Do not refire.** Surface to the user with a severity-trend table.

### 2B.9.1 — MAX_ROUNDS severity surface

When you hit MAX_ROUNDS, the user needs to choose: ship, defer, or explicitly override the cap. Give them the data to decide. Build a per-round severity-and-outcome table by walking each panel-id in your cycle state:

```bash
ROUND=0
for panel_id in <your-list-of-panel-ids>; do
  ROUND=$((ROUND + 1))
  fpath=~/.orchestra/panels/$panel_id/dispositions.json
  if [ ! -f "$fpath" ]; then continue; fi
  jq -r --arg pid "$panel_id" --argjson r "$ROUND" '
    ([.dispositions[] | .outcome] | group_by(.) | map({(.[0]): length}) | add // {})
    + {panel_id: $pid, round: $r}
  ' "$fpath"
done
```

(Adapt to your needs — the goal is to print a table, not run that exact pipeline. You can also `jq` over the per-round findings JSON to get severity counts since dispositions don't carry severity.)

Surface a table like this in your response (the format is what matters; build it however):

```
Cycle summary (3 rounds)

Round | High | Med | Low | fixed | ack | fp | scope
------|------|-----|-----|-------|-----|----|-----------
   1  |   5  |  10 |   8 |   18  |  5  |  0 | staged
   2  |   2  |   4 |   3 |    8  |  1  |  0 | tree:abc12
   3  |   0  |   1 |  14 |    9  |  6  |  0 | tree:def34

Remaining (round 3 fixed but unfired): 0 H / 1 M / 14 L
```

Then ask the user with `AskUserQuestion`:

1. **Ship with current fixes.** Round-3 fixes are applied; write the marker, retry the commit. The 14 LOWs aren't refired against. (Recommended when remaining is L-heavy.)
2. **Acknowledge and ship.** Update round-3 dispositions to `acknowledged` for the items you don't want to fix, then write the marker.
3. **Defer to a follow-up PR.** Stage your current fixes, commit; open a follow-up PR for the remaining findings.
4. **Continue past MAX_ROUNDS.** Apply the remaining fixes (per 2B.8), capture the post-fix tree (per 2B.9), then refire one more round with `--scope tree:$POST_FIX_TREE` (per 2B.2). The MAX_ROUNDS cap effectively becomes a soft cap once the user opts in; surface the severity table again after the next round and ask the same question. Only choose this if a HIGH/CRITICAL is in the remaining list — if the table shows zero HIGH+CRITICAL, this option is almost always wrong.

Include the override list (your `deduper_override: true` dispositions across all rounds) in your response so the user can audit.

## Async handling rules

- **Always validate `final.md` not just `exit_code`.** Exit 0 with an empty `final.md` is a silent failure, not "no findings."
- **Read job dirs by job-id, not "the latest dir".** Multiple panels can be running concurrently in the same session if you triggered them.
- **Never write the marker on a partial panel.** All 6 lens jobs must have exit_code == 0 with valid `final.md` before any marker is touched.
- **Never write a pre-fix marker for a post-fix tree (or vice versa).** Recompute `git write-tree` immediately before `touch`-ing the marker.

## Defaults

| Parameter      | Value                    | Rationale                                                                                  |
| -------------- | ------------------------ | ------------------------------------------------------------------------------------------ |
| Lens count     | always 6                 | The panel is the whole point of v2 — don't drop lenses heuristically                       |
| Lens tier      | `strongest`              | Opus xhigh + Codex high (local codex caps at `high`) — anything weaker defeats the purpose |
| Deduper tier   | `strongest` (Opus xhigh) | Same tier as Claude lenses; better merge + FP judgment                                     |
| MAX_ROUNDS     | 3                        | Hard ceiling; surface to user past this                                                    |
| Trivial bypass | always classify first    | Doc fixes don't deserve a $5 panel                                                         |

## Anti-patterns

- ❌ **Generic packet.** "Just review this diff" → generic results. Fill in every section of the packet template.
- ❌ **Fire and forget.** Always come back for the panel result before committing.
- ❌ **Ignoring low/medium findings without recording a disposition.** Every valid finding gets an outcome in `dispositions.json` — `fixed`, `acknowledged`, or `false_positive`. Silently dropping a finding (no fix, no entry) is the anti-pattern. `acknowledged` with a defensible reason is a legitimate outcome; "I'm just going to skip this" is not.
- ❌ **Blindly applying the deduper's FP labels.** Verify each rejection's reason has code citations.
- ❌ **Skipping FP verification because "the panel seems clean."** If the deduper reports `rejected_count > 0`, you read every rejection.
- ❌ **Treating a non-zero `exit_code` as "no findings — looks clean."** Inspect `stderr.log` first.
- ❌ **Writing the marker on partial-panel completion.** All 6 lenses must finish cleanly.
- ❌ **Reusing a pre-fix tree hash as a post-fix marker.** Recompute `git write-tree` after `git add -u`.
- ❌ **Inlining the packet or deduper prompt as a heredoc.** Always write to a file and pass via `--prompt-file` / `--packet`.
- ❌ **Calling a non-trivial diff "trivial" to take the bypass.** The classification rule is "no semantic logic change." Multi-line edits, control-flow changes, new functions, schema/API changes, behavior changes are NEVER trivial regardless of line count.
- ❌ **Bypassing the hook by wrapping the commit in a shell form the parser doesn't reach** (heredoc-to-bash, deeply nested subshells, `eval`). If you find yourself reaching for a wrapper to dodge the block, that's the signal to actually run the panel.
- ❌ **Skipping classification in autonomous mode.** Trivial diffs still get the bypass; the autonomous distinction only changes whether you ASK the user about non-trivial panels — it doesn't promote trivial diffs into reviewable ones.
- ❌ **Letting fixer scope creep happen.** Each fix is scoped to its finding. Adjacent "improvements" are out of scope and create new findings the next round will catch, looping forever.
- ❌ **Hiding overrides from the user.** Every disposition entry with `deduper_override: true` must be surfaced in your final response with its reason — so the user can audit cases where you disagreed with the deduper's verdict.
- ❌ **Cheating MAX_ROUNDS.** Three rounds is the cap. If you can't converge, surface to the user — don't keep firing panels.
- ❌ **Chaining `git add` and `git commit` in the same Bash call.** The hook fires as `PreToolUse` and blocks the entire command — `git add` never runs either. Always issue them as separate Bash tool calls: stage first, confirm with `git diff --cached --stat`, then commit.
- ❌ **Using `ScheduleWakeup` to wait for panel or deduper jobs.** These are background tasks — the harness notifies you automatically when they complete. `ScheduleWakeup` is for `/loop dynamic` mode only. Never fire it inside a review-pipeline run.
- ❌ **Round 2+ with `--scope staged`.** Defeats the frozen baseline. Round 1 reviews the full diff; every subsequent round MUST use `--scope tree:<previous-post-fix-tree>` so the review surface shrinks each round instead of growing. The growing-surface failure mode is what caused the NA-1058 10-round loop.
- ❌ **Refiring at MAX_ROUNDS without surfacing the severity table.** The user cannot make an informed continue/stop decision without per-round H/M/L counts and fixed/ack/fp breakdown. Build the table first, then ask.
- ❌ **Using `acknowledged` as a synonym for "I don't feel like fixing this".** The `reason` must be defensible to a reviewer. Out-of-scope, deferred to a tracked follow-up, or spec-mandated are valid. "Low impact" without justification is not.
- ❌ **Overriding the deduper without setting `deduper_override: true`.** If you re-classified a deduper-marked FP as valid (or vice versa), the disposition entry MUST flag the override for audit. This is how the user spots cases where the deduper and the conductor systematically disagree.
