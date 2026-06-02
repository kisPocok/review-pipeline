#!/usr/bin/env bash
# install.sh — install review-pipeline into ~/.claude/skills/.
#
# Usage:
#   ./install.sh                              # Option A: global hook, fires everywhere
#   ./install.sh --project <repo-path>        # Option B: hook scoped to one project
#
# Idempotent:
#   1. Builds the Go hook binary.
#   2. Symlinks the whole repo into ~/.claude/skills/review-pipeline.
#   3. Registers the hook in either
#        ~/.claude/settings.json                    (default — global)
#      or
#        <project>/.claude/settings.local.json      (with --project, scoped)
#      Preserves existing hooks; idempotent.
#   4. Ensures ~/.orchestra/{jobs,panels,markers}/ exist.
#   5. Prints next-steps.

set -euo pipefail

PROJECT_ROOT=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)  PROJECT_ROOT="$2"; shift 2 ;;
    -h|--help)  sed -n '2,18p' "$0"; exit 0 ;;
    *) printf 'install.sh: unknown flag: %s\n' "$1" >&2; exit 2 ;;
  esac
done

SOURCE_ROOT=$(cd "$(dirname "$0")" && pwd)
TARGET_ROOT="$HOME/.claude/skills/review-pipeline"
HOOK_BIN_REL="hook/bin/pre-commit-check"

if [[ -n "$PROJECT_ROOT" ]]; then
  PROJECT_ROOT=$(cd "$PROJECT_ROOT" 2>/dev/null && pwd) \
    || { printf 'install.sh: --project path does not exist: %s\n' "$PROJECT_ROOT" >&2; exit 2; }
  SETTINGS="$PROJECT_ROOT/.claude/settings.local.json"
  SCOPE_LABEL="project ($PROJECT_ROOT)"
else
  SETTINGS="$HOME/.claude/settings.json"
  SCOPE_LABEL="global (~/.claude/settings.json)"
fi

log() { printf '[install] %s\n' "$*"; }
fail() { printf '[install] error: %s\n' "$*" >&2; exit 1; }

# 0. Preflight: required external tools.
# fswatch is used by SKILL.md 2B.3 to event-wait on lens jobs (no sleep loops).
command -v fswatch >/dev/null 2>&1 \
  || fail "fswatch not found on PATH. Install with: brew install fswatch (macOS) or apt install fswatch (Debian/Ubuntu)."

# 1. Build the hook binary.
log "building $HOOK_BIN_REL"
( cd "$SOURCE_ROOT" && go build -o "$HOOK_BIN_REL" ./hook/cmd/pre-commit-check )
[[ -x "$SOURCE_ROOT/$HOOK_BIN_REL" ]] || fail "build did not produce $HOOK_BIN_REL"

# 2. Symlink the whole source dir into ~/.claude/skills/.
#
# One symlink rather than per-file symlinks inside a real target dir. Simpler,
# idempotent, and tolerates an existing symlink from a prior install that
# already points at this source path.
mkdir -p "$(dirname "$TARGET_ROOT")"
if [[ -L "$TARGET_ROOT" ]]; then
  current=$(readlink "$TARGET_ROOT")
  if [[ "$current" != "$SOURCE_ROOT" ]]; then
    log "replacing stale symlink $TARGET_ROOT (was -> $current)"
    rm "$TARGET_ROOT"
    ln -s "$SOURCE_ROOT" "$TARGET_ROOT"
  else
    log "$TARGET_ROOT already symlinked to source — leaving it"
  fi
elif [[ -e "$TARGET_ROOT" ]]; then
  fail "$TARGET_ROOT exists and is not a symlink — move it aside and re-run."
else
  log "symlinking $SOURCE_ROOT -> $TARGET_ROOT"
  ln -s "$SOURCE_ROOT" "$TARGET_ROOT"
fi

# 3. Patch the chosen settings file to register the hook.
# Use jq to add a PreToolUse Bash entry without disturbing existing hooks.
# Idempotent: if an entry pointing at our binary already exists, do nothing.
HOOK_PATH="$TARGET_ROOT/$HOOK_BIN_REL"
if [[ ! -f "$SETTINGS" ]]; then
  mkdir -p "$(dirname "$SETTINGS")"
  echo '{}' > "$SETTINGS"
fi

log "registering hook ($SCOPE_LABEL)"
TMP=$(mktemp)
HOOK_CMD="$HOOK_PATH" jq '
  . as $root
  | (.hooks // {}) as $h
  | (($h.PreToolUse // [])) as $pre
  | (env.HOOK_CMD) as $cmd
  | ($pre | any(.matcher == "Bash" and (.hooks // [] | any(.command == $cmd)))) as $present
  | if $present then
      $root
    else
      $root | .hooks = (
        ($h | .PreToolUse = (
          ($pre | map(
            if .matcher == "Bash" then
              .hooks = ((.hooks // []) + [{type: "command", command: $cmd}])
            else . end
          )) as $updated
          | if ($updated | any(.matcher == "Bash")) then
              $updated
            else
              $updated + [{matcher: "Bash", hooks: [{type: "command", command: $cmd}]}]
            end
        ))
      )
    end
' "$SETTINGS" > "$TMP"
mv "$TMP" "$SETTINGS"

# 4. Ensure orchestra dirs exist with safe perms.
mkdir -p "$HOME/.orchestra/jobs/codex" "$HOME/.orchestra/jobs/claude" "$HOME/.orchestra/panels"
mkdir -p "$HOME/.orchestra/markers"
chmod 700 "$HOME/.orchestra/markers"

# 5. Next steps.
cat <<EOF

[install] done.

  source         : $SOURCE_ROOT
  installed at   : $TARGET_ROOT
  hook binary    : $HOOK_PATH
  scope          : $SCOPE_LABEL
  settings file  : $SETTINGS

Next steps:
  - Verify the hook by running:
      go test ./... -count=1   (in $SOURCE_ROOT)
  - Trigger a real commit and confirm the hook blocks with the expected stderr.
  - For codex API-key auth: ensure ~/.codex/ is configured (run \`codex login\`).
  - For claude subscription auth: ensure \`claude\` CLI is logged in (run \`claude\` once interactively).

Disable: remove the entry pointing at $HOOK_PATH from $SETTINGS$([[ -z "$PROJECT_ROOT" ]] && echo " and delete $TARGET_ROOT" || true).
EOF
