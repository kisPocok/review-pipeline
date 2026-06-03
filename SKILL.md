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

### 2B.2 — Fire the panel

```bash
PANEL_ID=$(~/.claude/skills/review-pipeline/panel/review-panel \
  --repo-root <effective_cwd> \
  --scope staged \
  --packet /tmp/review-pipeline-packet-<slug>.md \
  --name <short-slug>)
```

`review-panel` fires all 6 lenses in parallel and prints the panel-id. The manifest lives at `~/.orchestra/panels/$PANEL_ID/manifest.json` and lists which job-id each lens went to.

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

### 2B.6 — Verify false-positive labels (sober second opinion)

Read every `verdict: false_positive` entry in `findings.json`. For each, look at `verdict_reason`:

- **Strong reason** (cites specific code that contradicts the finding): keep `false_positive`.
- **Weak reason** ("probably fine", "could be intentional", "may be a known pattern", or anything not code-grounded): **promote back to `valid`**. The deduper still merges structurally — the safety net is you.

For each promotion, record the override: lens + finding title + the weak reason + your decision. Surface the override list to the user at the end of the loop alongside the fixes.

### 2B.7 — Decide

- If the count of `valid` findings (after your verification pass) is **0** → write the marker for the current staged tree (which is the pre-fix tree) with `~/.claude/skills/review-pipeline/panel/write-marker <effective_cwd> [git globals]`. **DONE.** Retry the commit; the hook will consume the marker.
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
- ❌ **Ignoring low/medium findings to save time.** If they're valid (after your FP verification), fix them.
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
- ❌ **Hiding overrides from the user.** If you promoted any false_positive back to valid, the user must see that in your final response.
- ❌ **Cheating MAX_ROUNDS.** Three rounds is the cap. If you can't converge, surface to the user — don't keep firing panels.
- ❌ **Chaining `git add` and `git commit` in the same Bash call.** The hook fires as `PreToolUse` and blocks the entire command — `git add` never runs either. Always issue them as separate Bash tool calls: stage first, confirm with `git diff --cached --stat`, then commit.
- ❌ **Using `ScheduleWakeup` to wait for panel or deduper jobs.** These are background tasks — the harness notifies you automatically when they complete. `ScheduleWakeup` is for `/loop dynamic` mode only. Never fire it inside a review-pipeline run.
