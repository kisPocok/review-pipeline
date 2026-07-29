// pre-commit-check is the Claude Code PreToolUse hook for review-pipeline.
//
// Reads the tool invocation payload from stdin, detects a real `git commit`
// in the Bash command, and either allows it (exit 0) or blocks it (exit 2,
// stderr explaining what the agent should do) based on whether a marker file
// exists for the current staged tree hash.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nu/review-pipeline/hook/internal/detect"
	"github.com/nu/review-pipeline/hook/internal/marker"
)

type payload struct {
	Cwd       string `json:"cwd"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

func main() {
	os.Exit(run(os.Stdin, os.Stderr))
}

func run(stdin io.Reader, stderr io.Writer) int {
	var p payload
	if err := json.NewDecoder(stdin).Decode(&p); err != nil {
		// Bad/missing JSON payload: nothing to check; allow.
		return 0
	}
	if p.ToolInput.Command == "" {
		return 0
	}

	baseCwd := p.Cwd
	if baseCwd == "" {
		if wd, err := os.Getwd(); err == nil {
			baseCwd = wd
		}
	}

	result := detect.Analyze(p.ToolInput.Command, baseCwd)
	if !result.IsGitCommit {
		return 0
	}

	// A shell-dependent path form (mixed-quoted tilde, ~user) means the hook
	// cannot know which directory the commit targets. Block WITHOUT touching
	// any marker: computing a hash from a guessed path would let a decoy repo
	// at the literal path authorize an unreviewed commit elsewhere.
	if result.CwdUnresolved {
		fmt.Fprintln(stderr, ambiguousCwdMessage())
		return 2
	}

	hash, err := writeTree(result.EffectiveCwd, result.GitGlobals)
	if err != nil || hash == "" {
		// Could not compute a hash (no index, git unavailable, etc.). Block
		// with a clear message — failing open here would let through commits
		// the hook is supposed to gate.
		fmt.Fprintln(stderr, blockMessage(result.EffectiveCwd, result.GitGlobals, "", fmt.Sprintf("write-tree failed: %v", err)))
		return 2
	}

	markerDir, err := defaultMarkerDir()
	if err != nil {
		fmt.Fprintln(stderr, blockMessage(result.EffectiveCwd, result.GitGlobals, hash, fmt.Sprintf("could not resolve marker dir: %v", err)))
		return 2
	}

	consumed, consumeErr := marker.Consume(markerDir, hash)
	if consumeErr != nil {
		fmt.Fprintln(stderr, blockMessage(result.EffectiveCwd, result.GitGlobals, hash, fmt.Sprintf("marker dir unsafe: %v", consumeErr)))
		return 2
	}
	if consumed {
		return 0
	}

	fmt.Fprintln(stderr, blockMessage(result.EffectiveCwd, result.GitGlobals, hash, ""))
	return 2
}

// writeTree runs `git [globals] -C <cwd> write-tree` and returns the hash.
// If globals is non-empty, those args (--git-dir, --work-tree) are passed
// instead of the -C form so the hash matches the commit's view of the index.
func writeTree(cwd string, globals []string) (string, error) {
	args := make([]string, 0, len(globals)+3)
	args = append(args, globals...)
	if len(globals) == 0 {
		args = append(args, "-C", cwd)
	}
	args = append(args, "write-tree")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// defaultMarkerDir returns the marker dir under the user's home directory,
// $HOME/.orchestra/markers, consolidating with the rest of review-pipeline's
// ephemeral state.
func defaultMarkerDir() (string, error) {
	if override := os.Getenv("REVIEW_PIPELINE_MARKER_DIR"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".orchestra", "markers"), nil
}

func ambiguousCwdMessage() string {
	var b strings.Builder
	b.WriteString("review-pipeline hook: a `git commit` uses an ambiguous working directory.\n\n")
	b.WriteString("The hook cannot determine which directory the commit targets: the\n")
	b.WriteString("cd / git -C / --git-dir / --work-tree value is computed at run time\n")
	b.WriteString("($VAR, $(...)), or carries a tilde whose expansion depends on the\n")
	b.WriteString("executing shell (mixed quoting like ~\"/x\"; ~user depends on user\n")
	b.WriteString("lookup), or the command manipulates HOME, hides the git subcommand\n")
	b.WriteString("behind an expansion, or nests too deep to analyze. No marker will be\n")
	b.WriteString("computed or consumed for this command.\n\n")
	b.WriteString("❌ STOP. Re-issue the command with explicit literal paths — an absolute\n")
	b.WriteString("path, or a plain unquoted ~/... without HOME manipulation — then follow\n")
	b.WriteString("the normal review flow.\n")
	return b.String()
}

func blockMessage(cwd string, globals []string, hash, extra string) string {
	globalsDisplay := strings.Join(globals, " ")
	if globalsDisplay == "" {
		globalsDisplay = "<none>"
	}
	hashDisplay := hash
	if hashDisplay == "" {
		hashDisplay = "<unknown — write-tree failed or no index>"
	}

	writeMarkerCmd := "~/.claude/skills/review-pipeline/panel/write-marker " + cwd
	if len(globals) > 0 {
		writeMarkerCmd += " " + strings.Join(globals, " ")
	}

	var b strings.Builder
	b.WriteString("review-pipeline hook: a real `git commit` is about to run.\n")
	fmt.Fprintf(&b, "  effective cwd: %s\n", cwd)
	fmt.Fprintf(&b, "  git globals:   %s\n", globalsDisplay)
	fmt.Fprintf(&b, "  staged tree:   %s\n", hashDisplay)
	if extra != "" {
		fmt.Fprintf(&b, "  note:          %s\n", extra)
	}
	b.WriteString("\n❌ STOP. Invoke the `review-pipeline` skill BEFORE this commit. It covers\n")
	b.WriteString("classifying the diff (trivial vs non-trivial), running the review panel,\n")
	b.WriteString("and applying fixes.\n\n")
	b.WriteString("When the skill says to write the marker, run exactly:\n\n")
	fmt.Fprintf(&b, "    %s\n\n", writeMarkerCmd)
	b.WriteString("then retry the commit. Do NOT build the marker path yourself by command-\n")
	b.WriteString("substituting `git write-tree` — command substitution trips a permission\n")
	b.WriteString("prompt no allowlist entry can suppress.\n\n")
	b.WriteString("The marker is keyed to the exact staged tree (under the same git globals\n")
	b.WriteString("the hook saw), single-use (consumed via atomic unlink), and owner-private.\n\n")
	b.WriteString("If you have already completed the review for the current staged tree in\n")
	b.WriteString("this conversation, just write the marker now and retry the commit.\n")
	return b.String()
}
