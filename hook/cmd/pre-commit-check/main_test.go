package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates a git repo with one staged file at dir, returns the
// write-tree hash of the staged index.
func initRepo(t *testing.T) (repo string, treeHash string) {
	t.Helper()
	repo = t.TempDir()
	mustGit(t, repo, "init", "-q")
	mustGit(t, repo, "config", "user.email", "test@example.com")
	mustGit(t, repo, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hi\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	mustGit(t, repo, "add", "a.txt")
	out := mustGit(t, repo, "write-tree")
	return repo, strings.TrimSpace(out)
}

func mustGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// withMarkerDir sets REVIEW_PIPELINE_MARKER_DIR for the test's duration.
func withMarkerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Setenv("REVIEW_PIPELINE_MARKER_DIR", dir)
	return dir
}

func TestRun_NonJSONStdin_Allows(t *testing.T) {
	stdin := strings.NewReader("not-json")
	var stderr bytes.Buffer
	if code := run(stdin, &stderr); code != 0 {
		t.Errorf("non-JSON stdin: exit=%d, want 0; stderr=%s", code, stderr.String())
	}
}

func TestRun_EmptyCommand_Allows(t *testing.T) {
	stdin := strings.NewReader(`{"cwd":"/tmp","tool_input":{"command":""}}`)
	var stderr bytes.Buffer
	if code := run(stdin, &stderr); code != 0 {
		t.Errorf("empty command: exit=%d, want 0", code)
	}
}

func TestRun_NotGitCommit_Allows(t *testing.T) {
	stdin := strings.NewReader(`{"cwd":"/tmp","tool_input":{"command":"ls -la"}}`)
	var stderr bytes.Buffer
	if code := run(stdin, &stderr); code != 0 {
		t.Errorf("ls -la: exit=%d, want 0; stderr=%s", code, stderr.String())
	}
}

func TestRun_RealCommitNoMarker_Blocks(t *testing.T) {
	repo, _ := initRepo(t)
	_ = withMarkerDir(t)

	payload := fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git commit -m x"}}`, repo)
	var stderr bytes.Buffer
	code := run(strings.NewReader(payload), &stderr)
	if code != 2 {
		t.Fatalf("commit without marker: exit=%d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "STOP") {
		t.Errorf("stderr missing STOP message; got: %s", stderr.String())
	}
}

func TestRun_RealCommitWithMarker_AllowsAndConsumes(t *testing.T) {
	repo, treeHash := initRepo(t)
	markerDir := withMarkerDir(t)

	markerPath := filepath.Join(markerDir, treeHash)
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	payload := fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git commit -m x"}}`, repo)
	var stderr bytes.Buffer
	code := run(strings.NewReader(payload), &stderr)
	if code != 0 {
		t.Fatalf("commit with marker: exit=%d, want 0; stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("marker should have been consumed; stat err=%v", err)
	}
}

func TestRun_RealCommitViaCdAndC_HonorsMarker(t *testing.T) {
	repo, treeHash := initRepo(t)
	markerDir := withMarkerDir(t)

	// Marker placed for the same tree hash.
	if err := os.WriteFile(filepath.Join(markerDir, treeHash), nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	cmd := fmt.Sprintf("cd %s && git commit -m x", repo)
	payload := fmt.Sprintf(`{"cwd":"/tmp","tool_input":{"command":%q}}`, cmd)

	var stderr bytes.Buffer
	code := run(strings.NewReader(payload), &stderr)
	if code != 0 {
		t.Errorf("cd-then-commit with marker: exit=%d, want 0; stderr=%s", code, stderr.String())
	}
}

func TestBlockMessage_PointsAtWriteMarker_NoCommandSubstitution(t *testing.T) {
	msg := blockMessage("/some/repo", []string{"--git-dir", "/some/repo/.git"}, "abc123", "")

	if strings.Contains(msg, "$(") {
		t.Errorf("block message contains $(…) command substitution — running it trips an unsuppressible permission prompt:\n%s", msg)
	}
	if !strings.Contains(msg, "panel/write-marker /some/repo --git-dir /some/repo/.git") {
		t.Errorf("block message must give the write-marker invocation with literal cwd and git globals:\n%s", msg)
	}
	if !strings.Contains(msg, "review-pipeline") || !strings.Contains(msg, "STOP") {
		t.Errorf("block message must tell the agent to STOP and invoke the review-pipeline skill:\n%s", msg)
	}
}

func TestBlockMessage_NoGlobals_BareWriteMarkerCall(t *testing.T) {
	msg := blockMessage("/some/repo", nil, "abc123", "")
	if !strings.Contains(msg, "panel/write-marker /some/repo\n") {
		t.Errorf("with no git globals the write-marker call should be just the cwd:\n%s", msg)
	}
}

func TestRun_RealCommitConsumeIsSingleUse(t *testing.T) {
	repo, treeHash := initRepo(t)
	markerDir := withMarkerDir(t)

	if err := os.WriteFile(filepath.Join(markerDir, treeHash), nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	payload := fmt.Sprintf(`{"cwd":%q,"tool_input":{"command":"git commit -m x"}}`, repo)

	// First attempt: marker present → allow + consume.
	var stderr1 bytes.Buffer
	if code := run(strings.NewReader(payload), &stderr1); code != 0 {
		t.Fatalf("first attempt: exit=%d, want 0; stderr=%s", code, stderr1.String())
	}

	// Second attempt with same tree: marker gone → block.
	var stderr2 bytes.Buffer
	if code := run(strings.NewReader(payload), &stderr2); code != 2 {
		t.Errorf("second attempt: exit=%d, want 2 (marker should be single-use)", code)
	}
}
