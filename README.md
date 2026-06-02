# review-pipeline

Multi-lens commit-time code review for Claude Code. Every non-trivial `git commit` triggers a 6-lens panel (3 Codex `gpt-5.5 high` + 3 Claude `opus xhigh`) running in parallel, deduped by Claude `sonnet xhigh`, with main Claude as the conductor and sober-second-opinion. Fix loop bounded at 3 rounds.

## What it does

When you (or Claude) run `git commit`, a Go `PreToolUse` hook intercepts and refuses to allow the commit until a marker file exists at `~/.orchestra/markers/<git-write-tree>`. The marker is the receipt that the staged tree has been reviewed.

Claude reads the hook's stderr, opens this skill's `SKILL.md`, and follows the playbook:

1. **Classify** the diff (trivial → marker-only bypass; non-trivial → full panel).
2. **Write a shared review packet** (Context / Design / Constraints / Specific concerns / Out of scope).
3. **Fire the panel** via `panel/review-panel` — six jobs in parallel, each lens picks a different angle:

   | Lens                   | Runner | Tier      |
   |---|---|---|
   | security               | Codex  | strongest |
   | architecture           | Codex  | strongest |
   | quality                | Codex  | strongest |
   | security_xcheck        | Claude | strongest |
   | frontend               | Claude | strongest |
   | test_effectiveness     | Claude | strongest |

4. **Wait** for all 6 `exit_code` files. Validate `final.md` is non-empty and has severity headers.
5. **Dedupe** with a Sonnet xhigh job that emits structured JSON: merged findings with `verdict: valid|false_positive` per item.
6. **Verify** every `false_positive` label — promote any with a weak reason back to `valid`. (The deduper is Sonnet; you are Opus.)
7. If no valid findings remain, **write the marker** and retry the commit.
8. Otherwise, **apply the fixes in-session** with your own Edit/Write tools. Re-stage with `git add -u`. Loop back to step 3, up to **MAX_ROUNDS=3**.
9. After MAX_ROUNDS, surface remaining findings to the user.

## Install

Pick one of the two scopes. The skill itself always installs globally (Claude Code only discovers skills under `~/.claude/skills/`); the difference is whether the **pre-commit hook** fires for every project on this machine or only for a chosen one.

### Prerequisites

- `go` ≥ 1.21
- `jq`
- `git`
- `codex` CLI logged in (API key, standard `~/.codex/`) — test with `codex --version`
- `claude` CLI logged in (subscription) — test by opening a new Claude Code session
- Optional: `gtimeout` (`brew install coreutils` on macOS) for per-job wall-clock timeouts

### Option A — Global (hook fires on every commit, anywhere)

```bash
cd ~/review-pipeline    # wherever you cloned it
./install.sh
```

What `install.sh` does, idempotently:

1. Builds the Go hook binary at `hook/bin/pre-commit-check`.
2. Symlinks the repo into `~/.claude/skills/review-pipeline/`.
3. Registers the hook in `~/.claude/settings.json` under `PreToolUse → Bash` (preserves existing hooks; refuses to add a duplicate).
4. Creates `~/.orchestra/{jobs,panels,markers}/` with safe perms.

After install, any `git commit` Claude attempts — in any cwd — will be intercepted.

### Option B — Project-scoped (hook fires only when working in a specific project)

Useful if you only want this active in one repo and not for every commit elsewhere. The hook is registered in `<project>/.claude/settings.local.json`, which is gitignored by default — teammates aren't affected.

```bash
cd ~/review-pipeline    # wherever you cloned it
./install.sh --project /path/to/your/project
```

That single command does everything: builds the binary, symlinks the skill globally (Claude Code only discovers skills under `~/.claude/skills/`), creates `~/.orchestra/` dirs, and writes the hook entry into `<project>/.claude/settings.local.json` only — your global `~/.claude/settings.json` is left untouched.

Re-run any time to add the hook to another project:

```bash
./install.sh --project /path/to/another/repo
```

Idempotent — re-running with the same project is a no-op.

### Verify

```bash
# Binary built and executable.
ls -la ~/.claude/skills/review-pipeline/hook/bin/pre-commit-check

# Skill discoverable.
cat ~/.claude/skills/review-pipeline/SKILL.md | head -3

# Hook registered (pick the scope you installed).
jq '.hooks.PreToolUse' ~/.claude/settings.json                       # Option A
jq '.hooks.PreToolUse' /path/to/your/project/.claude/settings.local.json  # Option B
```

Open a **new** Claude Code session (so it picks up the registered hook) and attempt a `git commit` against a non-trivial diff. The hook should print:

```
review-pipeline hook: a real `git commit` is about to run.
  effective cwd: …
  git globals:   …
  staged tree:   …

STOP. Invoke the `review-pipeline` skill BEFORE this commit.
```

Claude will then read `SKILL.md` and follow the playbook.

## Disable

### Option A — Global

Remove the hook entry from `~/.claude/settings.json`:

```bash
HOOK_PATH="$HOME/.claude/skills/review-pipeline/hook/bin/pre-commit-check"
TMP=$(mktemp)
HOOK_CMD="$HOOK_PATH" jq '
  .hooks.PreToolUse |= map(.hooks |= map(select(.command != env.HOOK_CMD)))
' ~/.claude/settings.json > "$TMP" && mv "$TMP" ~/.claude/settings.json

# Optionally remove the skill itself.
rm ~/.claude/skills/review-pipeline
```

### Option B — Project-scoped

```bash
PROJECT="/path/to/your/project"
HOOK_PATH="$HOME/.claude/skills/review-pipeline/hook/bin/pre-commit-check"
SETTINGS="$PROJECT/.claude/settings.local.json"
TMP=$(mktemp)
HOOK_CMD="$HOOK_PATH" jq '
  .hooks.PreToolUse |= map(.hooks |= map(select(.command != env.HOOK_CMD)))
' "$SETTINGS" > "$TMP" && mv "$TMP" "$SETTINGS"
```

The skill symlink at `~/.claude/skills/review-pipeline` can stay — without a registered hook, nothing fires. To disable across multiple projects, repeat the above for each project's settings file.

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
│   └── claude-job                          # async claude -p primitive (subscription auth)
├── panel/
│   ├── review-panel                        # fires 6 lens jobs, writes manifest
│   └── lenses.sh                           # static 6-lens config
├── lenses/
│   ├── security.md
│   ├── architecture.md
│   ├── quality.md
│   ├── security_xcheck.md
│   ├── frontend.md
│   └── test_effectiveness.md
├── triage/
│   ├── deduper-prompt.md                   # merge + classify instructions
│   └── deduper-schema.json                 # JSON schema for findings.json
└── go.mod / go.sum                         # mvdan.cc/sh/v3 for the hook
```

## Costs

- **Codex** is API-billed (3 reviewers × `gpt-5.5 high` per round — local codex caps at `high`). Rough per-commit cost on a 200–500 line diff: **$2–8 per round**.
- **Claude** is free under your subscription (3 reviewers + 1 deduper per round). Shares rate limits with the main Claude session.

Per-commit total ≈ $2–8 × number-of-rounds. MAX_ROUNDS=3 caps the worst case.

## Trust model

- Hook is stateless. The only state it touches is the single-use marker file.
- Hook validates marker dir is owned by current UID with mode 0o700 before consuming. Refuses unsafe dirs.
- Subprocesses (`codex exec`, `claude -p`) inherit the user's auth; no credentials touched by the hook or job scripts.
- `codex-job` uses standard `~/.codex/` (API key).
- `claude-job` strips `ANTHROPIC_API_KEY` to force subscription auth.

## Testing

```bash
go test ./... -count=1
```

72 tests across `hook/internal/detect`, `hook/internal/marker`, and `hook/cmd/pre-commit-check`. The detect tests cover every BLOCK / ALLOW case from the inspiration kit's smoke-tests, ported as Go table-driven cases.
