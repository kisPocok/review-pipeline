# review-pipeline

Commit-time code review for Claude Code. Every non-trivial `git commit` triggers two reviewers (one Codex, one Claude, in parallel) over a shared prompt — a fixed preamble plus a per-diff packet. Main Claude reconciles both reviews directly and applies fixes. Fix loop bounded at 3 rounds.

A Go `PreToolUse` hook blocks `git commit` until `~/.orchestra/markers/<git-write-tree>` exists. Claude reads the hook stderr, opens `SKILL.md`, runs the reviewers, reconciles and applies fixes, writes the marker, retries.

## Reviewers

Two reviewers run in parallel over the same prompt; diversity comes from the models, not differing instructions.

| Reviewer | Runner | Tier      |
|----------|--------|-----------|
| codex    | Codex  | strongest |
| claude   | Claude | strongest |

Both prompts begin with a fixed preamble (`panel/reviewer-preamble.md`) covering four standing concerns — correctness, readability/maintainability, test quality, security — and the severity output format. Main Claude reads both reviews, reconciles overlaps and false positives itself, and records a per-finding outcome (`fixed` / `acknowledged` / `false_positive`) in `triage/disposition-schema.json`.

## Install

Prereqs: `go ≥ 1.21`, `jq`, `git`, `codex` CLI (API-key auth, `~/.codex/`), `claude` CLI (subscription auth). Optional: `gtimeout` (`brew install coreutils`).

```bash
./install.sh                              # global: hook fires on every commit
./install.sh --project /path/to/repo      # scoped: hook only in that repo
```

`install.sh` builds the hook binary, symlinks the repo into `~/.claude/skills/review-pipeline/`, creates `~/.orchestra/{jobs,panels,markers}/`, and registers the hook in:

- `~/.claude/settings.json` (default), or
- `<project>/.claude/settings.local.json` (with `--project`, gitignored — teammates unaffected).

Idempotent; re-run any time to add another project.

**Auth:** `codex-job` uses `~/.codex/` (API-billed). `claude-job` strips `ANTHROPIC_API_KEY` to force subscription auth (free, shares rate limits with main session).

## Autonomous runs (permissions)

Clearing the hook runs `preflight`, the panel scripts, `jq`, and `git`, and reads/writes under `~/.orchestra` and `/tmp`. To run the loop without a permission prompt at each step, add these to the **same settings file you registered the hook in** — global `~/.claude/settings.json`, or `<project>/.claude/settings.local.json` when installed with `--project`:

```json
{
  "permissions": {
    "allow": [
      "Bash(~/.claude/skills/review-pipeline/panel/preflight)",
      "Bash(~/.claude/skills/review-pipeline/panel/review-panel:*)",
      "Bash(~/.claude/skills/review-pipeline/panel/wait-panel:*)",
      "Bash(~/.claude/skills/review-pipeline/panel/write-marker:*)",
      "Bash(jq:*)",
      "Read(~/.orchestra/**)",
      "Write(~/.orchestra/**)",
      "Write(/tmp/review-pipeline-packet-*.md)"
    ]
  }
}
```

`preflight` takes **no arguments**, so list it as an exact match. A trailing-glob pattern (`… /preflight *`) requires an argument and silently fails to match the bare call — the result is a prompt on the skill's mandatory first action, every run.

The pipeline also runs `git` (`diff`, `add`, `status`, `write-tree`, `commit`). Allow these to taste: `Bash(git:*)` is simplest, or scope to those subcommands if you'd rather not auto-allow destructive git in the repo. (A wrapper that auto-allows rewritten git — e.g. rtk — can cover these without an explicit entry.)

## Layout

```
review-pipeline/
├── install.sh                              # build + symlink + register hook
├── SKILL.md                                # playbook for main Claude
├── hook/
│   ├── cmd/pre-commit-check/main.go        # Go hook entry point
│   ├── internal/detect/                    # shell-AST git-commit detection
│   ├── internal/marker/                    # marker dir + atomic consume
│   └── bin/pre-commit-check                # built binary (gitignored)
├── jobs/
│   ├── codex-job                           # async codex exec primitive
│   └── claude-job                          # async claude -p primitive
├── panel/
│   ├── preflight                           # warm-up permissions / deps
│   ├── review-panel                        # fires 2 reviewer jobs, writes manifest
│   ├── wait-panel                          # polls until both exit_code files land
│   ├── write-marker                        # write-tree + touch marker
│   └── reviewer-preamble.md               # fixed preamble: 4 concerns + output format
├── triage/
│   └── disposition-schema.json             # per-finding outcomes recorded by main Claude
└── go.mod / go.sum                         # mvdan.cc/sh/v3 for the hook
```
