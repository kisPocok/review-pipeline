package detect

import "testing"

// TestAnalyze_IsGitCommit_BlockCases mirrors peer's "real commits should BLOCK"
// section from hooks/smoke-tests.sh.
func TestAnalyze_IsGitCommit_BlockCases(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
	}{
		{"plain", "git commit -m x"},
		{"git -C path commit", "git -C /tmp/foo commit -am test"},
		{"git -c k=v commit", "git -c user.name=foo commit -m x"},
		{"--git-dir --work-tree commit", "git --git-dir /tmp/.git --work-tree /tmp commit -m x"},
		{"--namespace commit", "git --namespace foo commit -m x"},
		{"--exec-path=/p commit (value form)", "git --exec-path=/usr/lib/git commit -m x"},
		{"VAR=val env-assign git commit", "FOO=bar git commit -m x"},
		{"cd path && git commit", "cd /tmp && git commit -m x"},
		{"git status; git commit", "git status; git commit -m x"},
		{"git status && git commit", "git status && git commit -m x"},
		{"unspaced git status&&git commit", "git status&&git commit -m x"},
		{"(cd p && git commit) subshell", "(cd /tmp && git commit -m x)"},
		{"(cd p); git commit (cd doesn't escape)", "(cd /tmp); git commit -m x"},
		{"echo `git commit` (backtick subst)", "echo `git commit -m x`"},
		{"echo $(git commit) (cmd subst)", "echo $(git commit -m x)"},
		{"process subst <(git commit)", "cat <(git commit -m x)"},
		{"newline-separated lines", "git status\ngit commit -m x"},
		{"backslash-newline continuation", "git \\\n commit -m x"},

		// Shell wrappers.
		{"bash -c 'git commit'", "bash -c 'git commit -m x'"},
		{"sh -c 'git commit'", "sh -c 'git commit -m x'"},
		{"eval 'git commit'", "eval 'git commit -m x'"},
		{"command git commit", "command git commit -m x"},
		{"env FOO=bar git commit", "env FOO=bar git commit -m x"},
		{"time git commit", "time git commit -m x"},
		{"heredoc bash <<EOF git commit", "bash <<EOF\ngit commit -m x\nEOF\n"},

		// Commit-message values that look like flags must not flip the verdict.
		{"git commit -m '--dry-run' (msg val)", "git commit -m --dry-run"},
		{"git commit -m \"--help\" (msg val)", "git commit -m --help"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Analyze(tt.cmd, "/tmp")
			if !got.IsGitCommit {
				t.Errorf("Analyze(%q) IsGitCommit = false; want true", tt.cmd)
			}
		})
	}
}

// TestAnalyze_IsGitCommit_AllowCases mirrors peer's "false-positive guards" and
// "info/help/dry-run handling" sections.
func TestAnalyze_IsGitCommit_AllowCases(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
	}{
		// False-positive guards.
		{"echo \"git commit\"", `echo "git commit"`},
		{"printf \"git commit\"", `printf "git commit"`},
		{"grep 'git commit' file", "grep 'git commit' file"},
		{"git status", "git status"},
		{"git -C path show", "git -C /tmp show"},
		{"ls -la", "ls -la"},
		{"cat <<EOF (text only, non-shell consumer)", "cat <<EOF\nplease run git commit later\nEOF\n"},

		// Info/help.
		{"git --help commit", "git --help commit"},
		{"git --html-path commit", "git --html-path commit"},
		{"git --version", "git --version"},
		{"git --exec-path commit (bare = info)", "git --exec-path commit"},

		// Dry-run / help on the commit subcommand.
		{"git commit --dry-run", "git commit --dry-run"},
		{"git commit --help", "git commit --help"},
		{"git commit -h", "git commit -h"},

		// -S / --gpg-sign take optional values; --dry-run after should still allow.
		{"git commit -S --dry-run (optional arg)", "git commit -S --dry-run"},
		{"git commit -S keyid --dry-run", "git commit -S keyid --dry-run"},
		{"git commit --gpg-sign --dry-run", "git commit --gpg-sign --dry-run"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Analyze(tt.cmd, "/tmp")
			if got.IsGitCommit {
				t.Errorf("Analyze(%q) IsGitCommit = true; want false", tt.cmd)
			}
		})
	}
}

// TestAnalyze_EffectiveCwd checks that cwd is correctly resolved through `cd`
// and `git -C` so that subsequent write-tree calls match the commit's view.
func TestAnalyze_EffectiveCwd(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		baseCwd string
		wantCwd string
	}{
		{
			name:    "plain commit uses baseCwd",
			cmd:     "git commit -m x",
			baseCwd: "/repo",
			wantCwd: "/repo",
		},
		{
			name:    "cd absolute then commit",
			cmd:     "cd /other && git commit -m x",
			baseCwd: "/repo",
			wantCwd: "/other",
		},
		{
			name:    "cd relative then commit",
			cmd:     "cd sub && git commit -m x",
			baseCwd: "/repo",
			wantCwd: "/repo/sub",
		},
		{
			name:    "git -C absolute commit",
			cmd:     "git -C /other commit -m x",
			baseCwd: "/repo",
			wantCwd: "/other",
		},
		{
			name:    "cd inside subshell does not escape",
			cmd:     "(cd /other); git commit -m x",
			baseCwd: "/repo",
			wantCwd: "/repo",
		},
		{
			name:    "earlier git -C status does not stick",
			cmd:     "git -C /other status; git commit -m x",
			baseCwd: "/repo",
			wantCwd: "/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Analyze(tt.cmd, tt.baseCwd)
			if !got.IsGitCommit {
				t.Fatalf("Analyze(%q) IsGitCommit = false; want true", tt.cmd)
			}
			if got.EffectiveCwd != tt.wantCwd {
				t.Errorf("Analyze(%q).EffectiveCwd = %q; want %q", tt.cmd, got.EffectiveCwd, tt.wantCwd)
			}
		})
	}
}

// TestAnalyze_GitGlobals checks that --git-dir / --work-tree from the commit
// segment are captured so they can be replayed on `git write-tree`.
func TestAnalyze_GitGlobals(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		wantGlobals []string
	}{
		{
			name:        "no globals",
			cmd:         "git commit -m x",
			wantGlobals: nil,
		},
		{
			name:        "--git-dir space form",
			cmd:         "git --git-dir /tmp/.git commit -m x",
			wantGlobals: []string{"--git-dir", "/tmp/.git"},
		},
		{
			name:        "--work-tree space form",
			cmd:         "git --work-tree /tmp commit -m x",
			wantGlobals: []string{"--work-tree", "/tmp"},
		},
		{
			name:        "both --git-dir and --work-tree",
			cmd:         "git --git-dir /tmp/.git --work-tree /tmp commit -m x",
			wantGlobals: []string{"--git-dir", "/tmp/.git", "--work-tree", "/tmp"},
		},
		{
			name:        "--git-dir= equals form",
			cmd:         "git --git-dir=/tmp/.git commit -m x",
			wantGlobals: []string{"--git-dir=/tmp/.git"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Analyze(tt.cmd, "/tmp")
			if !got.IsGitCommit {
				t.Fatalf("Analyze(%q) IsGitCommit = false; want true", tt.cmd)
			}
			if !stringSlicesEqual(got.GitGlobals, tt.wantGlobals) {
				t.Errorf("Analyze(%q).GitGlobals = %v; want %v", tt.cmd, got.GitGlobals, tt.wantGlobals)
			}
		})
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
