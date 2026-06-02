# lenses.sh — static lens configuration for review-pipeline.
#
# Sourced by review-panel. Each entry: name|runner|tier|template
#
# runner ∈ {codex, claude}
# tier   ∈ {strongest, strong, standard, light} (codex also accepts codex-tuned)
# template is a path relative to the repo root (resolved to <skill>/lenses/<name>.md).
#
# The default panel is always all 6 lenses at strongest tier (3 Codex, 3 Opus).

LENSES=(
  "security|codex|strongest|security.md"
  "architecture|codex|strongest|architecture.md"
  "quality|codex|strongest|quality.md"
  "security_xcheck|claude|strongest|security_xcheck.md"
  "frontend|claude|strongest|frontend.md"
  "test_effectiveness|claude|strongest|test_effectiveness.md"
)
