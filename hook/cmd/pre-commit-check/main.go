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

func blockMessage(cwd string, globals []string, hash, extra string) string {
	globalsDisplay := strings.Join(globals, " ")
	if globalsDisplay == "" {
		globalsDisplay = "<none>"
	}
	hashDisplay := hash
	if hashDisplay == "" {
		hashDisplay = "<unknown — write-tree failed or no index>"
	}

	writeTreeCmd := "git " + strings.Join(globals, " ")
	if len(globals) == 0 {
		writeTreeCmd = "git "
	} else {
		writeTreeCmd += " "
	}
	writeTreeCmd += fmt.Sprintf("-C %s write-tree", cwd)

	var b strings.Builder
	b.WriteString("review-pipeline hook: a real `git commit` is about to run.\n")
	fmt.Fprintf(&b, "  effective cwd: %s\n", cwd)
	fmt.Fprintf(&b, "  git globals:   %s\n", globalsDisplay)
	fmt.Fprintf(&b, "  staged tree:   %s\n", hashDisplay)
	if extra != "" {
		fmt.Fprintf(&b, "  note:          %s\n", extra)
	}
	b.WriteString("\nSTOP. Invoke the `review-pipeline` skill BEFORE this commit.\n\n")
	b.WriteString("  - Interactive mode  → ask the user via AskUserQuestion whether to run the\n")
	b.WriteString("                        review panel first; honor the answer.\n")
	b.WriteString("  - Autonomous mode (multi-file implementation, or /goal invocation) →\n")
	b.WriteString("                        run the review panel automatically without asking.\n\n")
	b.WriteString("After processing the review and applying valid fixes (or after the user opts\n")
	b.WriteString("out), AND BEFORE re-running git commit, mark the post-fix staged tree as\n")
	b.WriteString("reviewed:\n\n")
	fmt.Fprintf(&b, "    MDIR=\"$HOME/.orchestra/markers\"\n")
	fmt.Fprintf(&b, "    install -d -m 700 \"$MDIR\"\n")
	fmt.Fprintf(&b, "    touch \"$MDIR/$(%s)\"\n\n", writeTreeCmd)
	b.WriteString("The marker is keyed to the exact staged tree (under the same git globals\n")
	b.WriteString("the hook saw), single-use (consumed via atomic unlink), and owner-private.\n\n")
	b.WriteString("If you have already completed the review for the current staged tree in\n")
	b.WriteString("this conversation, just write the marker now and retry the commit.\n")
	return b.String()
}
