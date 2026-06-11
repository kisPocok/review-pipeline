---
name: review-pipeline
description: Use when the user asks for a code review, or when the pre-commit / PreToolUse hook blocks a `git commit` on this repo. Triggers on phrases like "review my changes", "check this diff", "review the branch", or any time a commit is intercepted and needs to clear before retrying.
---

# review-pipeline

A two-reviewer commit-time review. The Go hook at `~/.claude/skills/review-pipeline/hook/bin/pre-commit-check` is registered as a `PreToolUse` on `Bash` and **blocks every real `git commit`** until a marker file exists at `~/.orchestra/markers/<git-write-tree>`. Your job when blocked: run the pre-flight warm-up, classify the diff, run the review pipeline if non-trivial, fix what's valid, write the marker for the final post-fix tree, retry the commit.

## At a glance

A map of the flow — **orientation only, not an action list.** Your first *action* is the pre-flight warm-up immediately below.

1. **Pre-flight warm-up** — always, before anything else.
2. **Classify** the staged diff — trivial or non-trivial?
   - **Trivial** → write the marker, retry the commit. Done.
   - **Non-trivial** → run the review loop:
     1. Write the shared packet (once per cycle).
     2. Fire the panel — round 1 `--scope staged`; round 2+ `--scope tree:<previous round's pre-fix tree>`.
     3. Wait for both reviewers; validate each `final.md`.
     4. Reconcile both reviews; assign a disposition to every finding.
     5. Nothing to fix → write the marker, retry the commit. **Done.** Otherwise apply fixes and re-stage; the next round's baseline is the `next baseline:` line wait-panel printed.
     6. Round < 3 → loop to step 2. Round == 3 → surface the severity table and ask the user.

## ⛔ FIRST ACTION — Pre-flight permission warm-up (DO NOT SKIP)

🛑 **STOP. This is the first thing you do when this skill is invoked.** Before reading the rest of this file, before looking at `git diff`, before classifying anything — run the block below. It surfaces every permission prompt the pipeline will trigger, upfront, in one shot. **Skip this and the async pipeline stalls mid-run waiting for approval the agent firing it cannot give**, silently — you will not get a useful error, just a hang.

This is not optional setup. This is not "if you have time". This is the first action. Run the command, confirm it prints `pre-flight OK`, then continue to Step 1 (classify the diff).

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

## Step 2B — Non-trivial path: two reviewers + fix loop

### 2B.1 — Write the shared review packet

Before firing the panel, write a one-time context packet to a temp file. `review-panel` assembles each reviewer's prompt as: fixed preamble (`panel/reviewer-preamble.md`, which states the four standing concerns — correctness, readability/maintainability, test quality, security — and the output format) + this packet + the scoped diff. Both reviewers get the identical prompt; the only variation is the model (Claude vs Codex).

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
- <e.g. `jq required at runtime (verified: command -v jq guard in panel/wait-panel)`>
- (or `None — standalone change`)

## Specific concerns to challenge
- <concern 1, framed as a question for the reviewers>
- Logic errors, missing edge cases, hidden assumptions
- Architectural soundness; simpler alternatives

## Out of scope
- <things the panel shouldn't flag — known lint warnings, WIP fixtures, follow-up TODOs>
- (or `None — flag anything you see`)
```

Save to `/tmp/review-pipeline-packet-<slug>.md`. **Never inline this as a heredoc** — always pass via `--packet <path>`. Every constraint or factual claim about the repo you write into the packet MUST be verified against the repo at packet-writing time — open the file and cite it. A constraint recalled from memory ("this script must stay POSIX-sh") that the repo contradicts sends both reviewers chasing a false premise.

### 2B.2 — Cycle concept (read before firing the panel)

A "cycle" is the sequence of rounds (panel → fix → panel → fix → ...) needed to clear one staged diff. Each round is its own panel-id. The review surface shrinks each round because the baseline advances:

- **Round 1** reviews the full diff (`--scope staged`, the default). This is the *original* feature being reviewed.
- **Round 2+** reviews **only the previous round's fix delta** — `--scope tree:<the tree round N-1 reviewed>`. `review-panel` records the tree it reviewed in the manifest (`reviewed_tree`, captured at fire time — before any fixes), and `wait-panel` prints it as a literal `next baseline: tree:<hash>` line on success. The diff is then exactly the fixes that round applied, so already-reviewed code is not re-flagged.

You will need to track in-session, per cycle:

- An array of panel-ids, one per round, in order
- The `next baseline: tree:<hash>` line from each round's wait-panel output — round N+1's `--scope` value
- For each panel-id, the path to its `dispositions.json` (written by you in 2B.7)

Use your TodoList (or scratch notes — the harness allows it) to hold this. There is no persistent cycle state file; the data is derivable from `~/.orchestra/panels/<panel-id>/` for any panel-id you remember.

### 2B.3 — Fire the panel

**Round 1** (the first panel of the cycle — reviews the full staged diff):

```bash
PANEL_ID=$(~/.claude/skills/review-pipeline/panel/review-panel \
  --repo-root <effective_cwd> \
  --scope staged \
  --packet /tmp/review-pipeline-packet-<slug>.md \
  --name <short-slug>-r1)
```

**Round 2+** (reviews only the previous round's fix delta):

```bash
PANEL_ID=$(~/.claude/skills/review-pipeline/panel/review-panel \
  --repo-root <effective_cwd> \
  --scope "tree:$BASELINE_TREE" \
  --packet /tmp/review-pipeline-packet-<slug>.md \
  --name <short-slug>-r<N>)
```

Where `$BASELINE_TREE` is the hash from the previous round's `next baseline: tree:<hash>` line (printed by wait-panel; also in that round's manifest as `reviewed_tree`). Do not recompute it with `git write-tree` yourself — by fix time the index has moved on. `review-panel` diffs `<BASELINE_TREE>..<current-write-tree>`; since the index now carries the previous round's fixes, that diff is exactly those fixes (plus any incidental staged changes since).

`review-panel` fires both reviewers (one `claude-job`, one `codex-job`) in parallel and prints the panel-id. The manifest lives at `~/.orchestra/panels/$PANEL_ID/manifest.json` under a `reviewers` object (`claude`, `codex`), each with its job-id. The manifest's `scope` field records the round's review scope (e.g., `tree:abc123…`) for audit.

**Why advance the baseline each round (not re-review `--cached`):** if round N re-reviewed the full `--cached` diff, the reviewers would re-flag every line of the original feature plus the fixes you applied — including new tests, helper functions, and guards added in response to round-N-1 findings. That's the runaway-iteration dynamic. By scoping to the previous round's fix delta, the per-round review surface shrinks instead of growing.

### 2B.4 — Wait for the panel and validate

One command polls for both reviewer `exit_code` files and validates each `final.md`:

```bash
~/.claude/skills/review-pipeline/panel/wait-panel "$PANEL_ID"
```

Behavior:
- Polls every 2s, logging only on state change (so stderr stays ~7 lines total regardless of total wait time).
- Validates: `exit_code` is `0`, `final.md` non-empty, contains either a severity header (`## Critical|High|Medium|Low`) or the literal `No findings.` sentinel, and carries the `## Trace log` header the reviewer protocol mandates.
- Exits `0` only if both reviewers pass. Non-zero exit → surface the summary table to the user; do **not** proceed to reconciliation.
- On success, prints the two `final.md` paths (one per reviewer) — use those in 2B.5 — and a final `next baseline: tree:<hash>` line. Record that line; it is the `--scope` value if a round N+1 fires (2B.3).

**If you have parallel work to do** (drafting the PR description, planning the next step), start that work after firing the panel. You'll be notified automatically when the background task completes — do NOT fire `ScheduleWakeup` for this.

### 2B.5 — Read and reconcile both reviews

`wait-panel` (2B.4) already printed the two `final.md` paths on success. **Open each one with the Read tool** (not Bash) — they live under `~/.orchestra/`, which `Read(~/.orchestra/**)` allows, so neither prompts.

> ⛔ Do **not** resolve the paths yourself with a shell pipeline like `CJ=$(jq … manifest.json); cat …/$CJ/final.md`. Command substitution (`$(…)`) and variable expansion (`$CJ`) trip Claude Code's `simple_expansion` analyzer, forcing a permission prompt that **no `allow` entry can suppress** — the dynamic command can't be statically matched. The paths from `wait-panel` are literal; just Read them.

You reconcile the two reviews yourself — no automated reconciliation step:

- Treat findings that name the same file+location and the same defect as one. Keep the clearer write-up.
- A finding raised by only one reviewer is still a finding — single-reviewer coverage does not lower its weight.
- Verify before trusting: open the cited code. A reviewer can be wrong; confirm the defect is real before classifying it.

### 2B.6 — Decide a disposition for every finding

For each distinct finding across the two reviews, assign a stable id (F001, F002, …), note its severity (from the `## Critical|High|Medium|Low` header it appeared under; if the two reviewers disagree, take the higher), and decide exactly one outcome:

| Outcome | When to use |
|---|---|
| `fixed` | You will apply a code change this round that addresses the finding. |
| `acknowledged` | Finding is real and valid, but you are accepting the risk in this PR (out-of-scope, deferred to follow-up, spec-mandated, etc.). Must justify in `reason` and ideally cite where the deferral is tracked (e.g., `docs/X/SECURITY-NOTES.md` or a Linear/Jira ticket). |
| `false_positive` | You read the cited code and it contradicts the finding. Cite that code in `reason`. |

**Solve all valid issues.** A finding that is valid and in-scope for this change MUST be fixed (`outcome: fixed`). The only non-fix outcomes are `acknowledged` (the finding is real but unrelated/out-of-scope to this change) and `false_positive` (the code contradicts it). Effort, severity, or "low gain" are never reasons to skip a valid in-scope finding.

### 2B.7 — Write the disposition file

Build and write `~/.orchestra/panels/$PANEL_ID/dispositions.json` matching `triage/disposition-schema.json`:

```json
{
  "panel_id": "<panel-id>",
  "round": <1-indexed>,
  "findings_path": "<~/.orchestra/panels/<panel-id>/ — the panel dir whose two reviews this covers>",
  "dispositions": [
    {"finding_id": "F001", "severity": "high", "outcome": "fixed", "reason": "applied input length cap in <file>:<line>"},
    {"finding_id": "F002", "severity": "medium", "outcome": "acknowledged", "reason": "pre-existing, unrelated to this change; tracked in docs/sso/SECURITY-NOTES.md"},
    {"finding_id": "F003", "severity": "low", "outcome": "false_positive", "reason": "guard already exists at routes.go:18; verified."}
  ]
}
```

Write the file before applying fixes (your decisions are made; the file records them). If you change your mind mid-fix (e.g., a `fixed` finding turns out to need a redesign), update the file to `acknowledged` with a reason before refiring the next round.

Then validate it and get the loop verdict in one command:

```bash
~/.claude/skills/review-pipeline/triage/check-dispositions "$PANEL_ID"
```

It validates every entry (finding_id `F###`, severity, outcome, non-empty reason — a missing `severity` would otherwise crash the 2B.11 table), prints per-outcome counts, and ends with a `verdict:` line that drives 2B.8. An empty `dispositions: []` is legitimate (a round where both reviewers returned `No findings.`) and verdicts as nothing-to-fix. Non-zero exit → the file is malformed; fix it and re-run before continuing.

### 2B.8 — Act on the verdict

`check-dispositions` (2B.7) ended with a `verdict:` line. Act on it:

**`verdict: GATE — N unfixed critical/high; ask the user`** — a critical or high finding you decided *not* to fix (`acknowledged` or `false_positive`) needs the user's sign-off before anything else, including any marker. You may be right, but you are also the author of the change being reviewed; the user audits that call before it ships silently. Surface each such finding (id, severity, outcome, your reason) via `AskUserQuestion` with options per finding-set: **Accept and ship** (keep the disposition, proceed), **Fix this round** (flip to `fixed`, proceed to 2B.9), or **Defer to a tracked follow-up** (keep `acknowledged`, add the tracker reference to `reason`). After the answer (and any disposition edits), re-run `check-dispositions` and act on the new verdict. Low/medium non-fix dispositions do not gate — the dispositions file records them for audit.

**`verdict: nothing to fix — write the marker and retry the commit`** → no fixes to apply. Write the marker for the current staged tree (`~/.claude/skills/review-pipeline/panel/write-marker <effective_cwd> [git globals]`). **DONE.** Retry the commit; the hook will consume the marker. The dispositions file records what you acknowledged or dismissed.

**`verdict: LOW-only fixes — surface to the user`** — every `outcome: fixed` finding is `severity: low`; STOP and use `AskUserQuestion` with three options:
  1. **Acknowledge and ship.** Change those LOW findings' dispositions from `fixed` to `acknowledged` (with a `reason` you write per finding), write the marker, retry the commit. (Recommended when the LOWs are test-tightening or comment fixes.)
  2. **Fix and ship.** Apply the LOW fixes this round, but write the marker after this round without refiring another panel. No round N+1.
  3. **Fix and continue.** Apply the LOWs and refire (standard loop). Only choose this if there's a concrete reason to expect new HIGHs hiding behind the LOW fixes.

  Surface the LOW finding titles in the question text so the user can judge.

**`verdict: proceed to fix — critical/high/medium findings remain`** → proceed to 2B.9 (apply fixes), then 2B.10 (re-stage and refire).

### 2B.9 — Apply fixes (in-session)

You apply the fixes yourself, using your own Edit/Write/Grep/Bash tools. No fixer subprocess.

**Fixer discipline (mandatory):**

> Apply exactly the changes the valid findings call for. Do not refactor unrelated code; do not improve naming/comments unless a finding cites it; do not add tests unless a finding requests them. After each fix, briefly note what changed (1 line) in your response to the user. Run typecheck / tests after each fix when the project supports it (look at the project's CLAUDE.md or `package.json` / `go.mod` for the command); if a fix breaks something, adjust and continue. If a **low/medium** finding cannot be addressed within the original change's scope (requires a redesign, a separate migration, out-of-scope test infrastructure, etc.) — defer it unilaterally: update its disposition to `acknowledged` with a one-line reason, note it in your response, and proceed. Do NOT use `AskUserQuestion` for low/medium deferrals. If a **critical/high** finding turns out to be unfixable mid-round, update its disposition and go back through the 2B.8 unfixed critical/high gate — flipping a critical/high from `fixed` to `acknowledged` after the gate already passed must not ship without the user's sign-off.

Process findings in severity order: critical → high → medium → low. Within a severity, file-group them to avoid multiple seeks to the same file.

Leave your edits in the working tree as you go; staging happens once, in 2B.10. (If a fix creates a *new* file, note it — `git add -u` won't pick it up; you'll add it explicitly in 2B.10.)

### 2B.10 — Re-stage and loop

Stage this round's fixes:

```bash
git -C <effective_cwd> add -u                        # stage modifications to tracked files
git -C <effective_cwd> add <new-file>...             # plus any new files a fix created
```

The next round's baseline is the `next baseline: tree:<hash>` line from this round's wait-panel output (2B.4) — `review-panel` recorded that tree at fire time, before any fixes existed, so it is always the correct pre-fix baseline. Round N+1 fires with that value as `--scope` (per 2B.3); since the index now carries this round's fixes, the diff is exactly the fixes you just applied. (`git add -u` stages only tracked-file changes — explicitly `git add` any new file a fix created, or it escapes review.)

The pre-fix marker (if any) is irrelevant — the tree has changed. Round counter += 1.

- **If round < MAX_ROUNDS (3)** → go back to step 2B.3 (fire round-N+1 panel with `--scope tree:$BASELINE_TREE`).
- **If round == MAX_ROUNDS** → STOP. **Do not refire.** Surface to the user with a severity-trend table.

### 2B.11 — MAX_ROUNDS severity surface

When you hit MAX_ROUNDS, the user needs to choose: ship, defer, or explicitly override the cap. Give them the data to decide. Build a per-round severity-and-outcome table by walking each panel-id in your cycle state:

```bash
ROUND=0
for panel_id in <your-list-of-panel-ids>; do
  ROUND=$((ROUND + 1))
  fpath=~/.orchestra/panels/$panel_id/dispositions.json
  if [ ! -f "$fpath" ]; then continue; fi
  jq -r --arg pid "$panel_id" --argjson r "$ROUND" '
    ([.dispositions[] | .outcome]  | group_by(.) | map({(.[0]): length}) | add // {})
    + ([.dispositions[] | .severity] | group_by(.) | map({(.[0]): length}) | add // {})
    + {panel_id: $pid, round: $r}
  ' "$fpath"
done
```

(Adapt to your needs — the goal is to print a table, not run that exact pipeline. Both the severity and the outcome counts come straight from each round's `dispositions.json` now that findings carry `severity`.)

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
4. **Continue past MAX_ROUNDS.** Apply the remaining fixes (per 2B.9), re-stage (per 2B.10), then refire one more round with the last wait-panel's `next baseline: tree:<hash>` as `--scope` (per 2B.3). The MAX_ROUNDS cap effectively becomes a soft cap once the user opts in; surface the severity table again after the next round and ask the same question. Only choose this if a HIGH/CRITICAL is in the remaining list — if the table shows zero HIGH+CRITICAL, this option is almost always wrong.

Include the list of `acknowledged` and `false_positive` dispositions across all rounds in your response so the user can audit the decisions.

## Async handling rules

- **Always validate `final.md`, not just `exit_code`.** Exit 0 with an empty `final.md` is a silent failure, not "no findings." On a non-zero `exit_code`, inspect `stderr.log` before doing anything else.
- **Read job dirs by job-id, not "the latest dir".** Multiple panels can be running concurrently in the same session if you triggered them.
- **Never write the marker on a partial panel.** Both reviewer jobs must have exit_code == 0 with valid `final.md` before any marker is touched.
- **Never write a pre-fix marker for a post-fix tree (or vice versa).** Recompute `git write-tree` immediately before `touch`-ing the marker.

## Defaults

| Parameter      | Value                    | Rationale                                                                                  |
| -------------- | ------------------------ | ------------------------------------------------------------------------------------------ |
| Reviewers      | 2 (1 Claude, 1 Codex)    | Model diversity over the same prompt; main agent reconciles                                |
| Reviewer tier  | `strongest`              | Opus xhigh + Codex xhigh — anything weaker defeats the purpose                              |
| MAX_ROUNDS     | 3                        | Hard ceiling; surface to user past this                                                    |
| Trivial bypass | always classify first    | Doc fixes don't deserve a $5 panel                                                         |

## Anti-patterns

- ❌ **Generic packet.** "Just review this diff" → generic results. Fill in every section of the packet template.
- ❌ **Fire and forget.** Always come back for the panel result before committing.
- ❌ **Ignoring low/medium findings without recording a disposition.** Every valid finding gets an outcome in `dispositions.json` — `fixed`, `acknowledged`, or `false_positive`. Silently dropping a finding (no fix, no entry) is the anti-pattern. `acknowledged` with a defensible reason is a legitimate outcome; "I'm just going to skip this" is not.
- ❌ **Inlining the packet as a heredoc.** Always write to a file and pass via --packet.
- ❌ **Reading the reviews via a shell pipeline with command substitution.** `CJ=$(jq … manifest.json); cat …/$CJ/final.md` trips the `simple_expansion` analyzer and prompts on every run — and no `allow` entry can suppress it, because the command is dynamic. `wait-panel` already prints the two literal `final.md` paths on success; just **Read** them (2B.5).
- ❌ **Calling a non-trivial diff "trivial" to take the bypass.** The classification rule is "no semantic logic change." Multi-line edits, control-flow changes, new functions, schema/API changes, behavior changes are NEVER trivial regardless of line count.
- ❌ **Bypassing the hook by wrapping the commit in a shell form the parser doesn't reach** (heredoc-to-bash, deeply nested subshells, `eval`). If you find yourself reaching for a wrapper to dodge the block, that's the signal to actually run the panel.
- ❌ **Skipping classification in autonomous mode.** Trivial diffs still get the bypass; the autonomous distinction only changes whether you ASK the user about non-trivial panels — it doesn't promote trivial diffs into reviewable ones.
- ❌ **Letting fixer scope creep happen.** Each fix is scoped to its finding. Adjacent "improvements" are out of scope and create new findings the next round will catch, looping forever.
- ❌ **Cheating MAX_ROUNDS.** Three rounds is the cap. If you can't converge, surface to the user — don't keep firing panels.
- ❌ **Chaining `git add` and `git commit` in the same Bash call.** The hook fires as `PreToolUse` and blocks the entire command — `git add` never runs either. Always issue them as separate Bash tool calls: stage first, confirm with `git diff --cached --stat`, then commit.
- ❌ **Using `ScheduleWakeup` to wait for panel jobs.** These are background tasks — the harness notifies you automatically when they complete. `ScheduleWakeup` is for `/loop dynamic` mode only. Never fire it inside a review-pipeline run.
- ❌ **Round 2+ with `--scope staged`.** Defeats the shrinking baseline. Round 1 reviews the full diff; every subsequent round MUST use `--scope tree:<previous round's pre-fix tree>` so the review surface shrinks each round instead of growing. The growing-surface failure mode is what caused the NA-1058 10-round loop.
- ❌ **Recomputing the next round's baseline with `git write-tree` yourself.** By the time fixes are staged, `write-tree` yields the post-fix tree — the next round would diff it against itself and `review-panel` exits with an empty-diff error. Always use the `next baseline: tree:<hash>` line wait-panel printed.
- ❌ **Refiring at MAX_ROUNDS without surfacing the severity table.** The user cannot make an informed continue/stop decision without per-round H/M/L counts and fixed/ack/fp breakdown. Build the table first, then ask.
- ❌ **Using `acknowledged` as a synonym for "I don't feel like fixing this".** The `reason` must be defensible to a reviewer. Out-of-scope, deferred to a tracked follow-up, or spec-mandated are valid. "Low impact" without justification is not.
- ❌ **Writing the marker while a critical/high finding sits at `acknowledged` or `false_positive` without user sign-off.** `fixed_count == 0` is not "done" — run the unfixed critical/high gate in 2B.8 first. Dismissing a CRITICAL is a user-visible decision, never a silent one.
