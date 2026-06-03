# review-pipeline

Commit-time code review for Claude Code. Every non-trivial `git commit` triggers a 6-lens panel (3 Codex + 3 Claude in parallel), deduped by Sonnet, with main Claude as conductor. Fix loop bounded at 3 rounds.

A Go `PreToolUse` hook blocks `git commit` until `~/.orchestra/markers/<git-write-tree>` exists. Claude reads the hook stderr, opens `SKILL.md`, runs the panel, applies fixes, writes the marker, retries.

## Lenses

| Lens               | Runner | Tier      |
|--------------------|--------|-----------|
| security           | Codex  | strongest |
| architecture       | Codex  | strongest |
| quality            | Codex  | strongest |
| security_xcheck    | Claude | strongest |
| frontend           | Claude | strongest |
| test_effectiveness | Claude | strongest |

Dedupe: Claude `sonnet xhigh` → structured JSON (`verdict: valid|false_positive`).

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
│   ├── review-panel                        # fires 6 lens jobs, writes manifest
│   ├── wait-panel                          # polls until all 6 exit_code files land
│   ├── run-dedupe                          # assembles reports, fires Sonnet deduper
│   ├── write-marker                        # write-tree + touch marker
│   └── lenses.sh                           # static 6-lens config
├── lenses/                                 # one prompt per lens
├── triage/
│   ├── deduper-prompt.md
│   └── deduper-schema.json
└── go.mod / go.sum                         # mvdan.cc/sh/v3 for the hook
```
