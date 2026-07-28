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

// TestAnalyze_ExpansionFailsClosed: expansions resolve to empty text during
// analysis, so a git subcommand hidden behind one is invisible. Like the
// maxRecursion case, that uncertainty must block, not allow.
func TestAnalyze_ExpansionFailsClosed(t *testing.T) {
	blocked := []struct {
		name string
		cmd  string
	}{
		{"variable subcommand", "C=commit; git $C -m x"},
		{"param-default subcommand", "git ${C:-commit} -m x"},
		{"cmdsubst subcommand", "git $(echo commit) -m x"},
		{"expansion before literal subcommand", "git $FLAGS commit -m x"},
		{"quoted expansion subcommand", `git "$C" -m x`},
		// Unquoted expansions word-split at runtime, so they can shift a
		// hidden commit into subcommand position even past a literal word.
		{"unquoted expansion in option value", "git -c color.ui=$COLOR status"},
		// Quoted array-style expansions stay quoted but still yield one argv
		// word per element — CFG=(color.ui=x commit) shifts a hidden commit
		// into subcommand position.
		{"quoted array expansion in option value", `git -c "${CFG[@]}" status`},
		{"quoted positional expansion in option value", `git -c "$@" status`},
		// Indirection can alias an [@] expansion (x='a[@]'), unresolvable
		// statically.
		{"quoted indirect expansion in option value", `git -c "${!CFG}" status`},
	}
	for _, tt := range blocked {
		t.Run(tt.name, func(t *testing.T) {
			got := Analyze(tt.cmd, "/tmp")
			if !got.IsGitCommit {
				t.Errorf("Analyze(%q) IsGitCommit = false; want true (fail closed)", tt.cmd)
			}
		})
	}

	allowed := []struct {
		name string
		cmd  string
	}{
		{"expansion after non-commit subcommand", "git log $REF"},
		{"expansion in commit-msg value still normal detection", "git status $X"},
		{"non-git command with expansions", "echo $C"},
		// Quoted expansions always stay a single word, so a literal
		// non-commit subcommand keeps its position — no reason to block.
		{"quoted expansion in -C value, literal status", `git -C "$REPO" status`},
		{"quoted expansion in -c value, literal status", `git -c "color.ui=$COLOR" status`},
		// Quoted scalar subscripts select one element ([*] joins) — only a
		// literal [@] subscript splits in quoted context.
		{"quoted indexed subscript, literal status", `git -c "${CFG[0]}" status`},
		{"quoted star subscript, literal status", `git -c "${CFG[*]}" status`},
	}
	for _, tt := range allowed {
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

// TestAnalyze_TildeExpansion checks that an unquoted leading tilde in cwd-
// affecting positions expands to $HOME exactly where every shell would, that
// forms all shells keep literal stay literal, and that forms whose meaning
// depends on the executing shell (mixed quoting, ~user) are flagged
// CwdUnresolved — a literal fallback there is itself a resolution, and a
// decoy repo at the literal path could be used to authorize the wrong tree.
func TestAnalyze_TildeExpansion(t *testing.T) {
	t.Setenv("HOME", "/home/tester")

	tests := []struct {
		name           string
		cmd            string
		baseCwd        string
		wantCwd        string
		wantUnresolved bool
		wantGlobals    []string
	}{
		{
			name:    "cd tilde-slash then commit",
			cmd:     "cd ~/profile && git commit -m x",
			baseCwd: "/repo",
			wantCwd: "/home/tester/profile",
		},
		{
			name:    "cd bare tilde then commit",
			cmd:     "cd ~ && git commit -m x",
			baseCwd: "/repo",
			wantCwd: "/home/tester",
		},
		{
			name:    "git -C tilde-slash commit",
			cmd:     "git -C ~/profile commit -m x",
			baseCwd: "/repo",
			wantCwd: "/home/tester/profile",
		},
		{
			name:    "single-quoted tilde stays literal",
			cmd:     "cd '~/profile' && git commit -m x",
			baseCwd: "/repo",
			wantCwd: "/repo/~/profile",
		},
		{
			name:    "double-quoted tilde stays literal",
			cmd:     `cd "~/profile" && git commit -m x`,
			baseCwd: "/repo",
			wantCwd: "/repo/~/profile",
		},
		{
			name:           "tilde-user form is unresolved",
			cmd:            "cd ~other/profile && git commit -m x",
			baseCwd:        "/repo",
			wantUnresolved: true,
		},
		{
			name:        "work-tree separate-word tilde value expands",
			cmd:         "git --work-tree ~/wt --git-dir /g/.git commit -m x",
			baseCwd:     "/repo",
			wantCwd:     "/repo",
			wantGlobals: []string{"--work-tree", "/home/tester/wt", "--git-dir", "/g/.git"},
		},
		{
			name:        "equals-joined tilde stays literal",
			cmd:         "git --work-tree=~/wt commit -m x",
			baseCwd:     "/repo",
			wantCwd:     "/repo",
			wantGlobals: []string{"--work-tree=~/wt"},
		},
		{
			name:           "mixed quoting tilde-then-quoted-slash is unresolved",
			cmd:            `cd ~"/x" && git commit -m x`,
			baseCwd:        "/repo",
			wantUnresolved: true,
		},
		{
			name:           "tilde-empty-quotes-slash is unresolved",
			cmd:            `cd ~""/x && git commit -m x`,
			baseCwd:        "/repo",
			wantUnresolved: true,
		},
		{
			name:    "unquoted tilde-slash with later quoted part expands",
			cmd:     `cd ~/"x" && git commit -m x`,
			baseCwd: "/repo",
			wantCwd: "/home/tester/x",
		},
		{
			name:           "bare tilde followed by quoted part is unresolved",
			cmd:            `cd ~"" && git commit -m x`,
			baseCwd:        "/repo",
			wantUnresolved: true,
		},
		{
			name:           "git -C mixed-quote tilde is unresolved",
			cmd:            `git -C ~"/x" commit -m x`,
			baseCwd:        "/repo",
			wantUnresolved: true,
		},
		{
			name:           "work-tree separate-word tilde-user is unresolved",
			cmd:            "git --work-tree ~other/wt commit -m x",
			baseCwd:        "/repo",
			wantUnresolved: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Analyze(tt.cmd, tt.baseCwd)
			if !r.IsGitCommit {
				t.Fatalf("Analyze(%q) IsGitCommit = false, want true", tt.cmd)
			}
			if r.CwdUnresolved != tt.wantUnresolved {
				t.Errorf("Analyze(%q) CwdUnresolved = %v, want %v", tt.cmd, r.CwdUnresolved, tt.wantUnresolved)
			}
			if tt.wantUnresolved {
				return
			}
			if r.EffectiveCwd != tt.wantCwd {
				t.Errorf("Analyze(%q) EffectiveCwd = %q, want %q", tt.cmd, r.EffectiveCwd, tt.wantCwd)
			}
			if tt.wantGlobals != nil && !stringSlicesEqual(r.GitGlobals, tt.wantGlobals) {
				t.Errorf("Analyze(%q) GitGlobals = %v, want %v", tt.cmd, r.GitGlobals, tt.wantGlobals)
			}
		})
	}
}

// TestAnalyze_AmbiguityPropagation checks that an ambiguous cd poisons every
// path that can detect a commit afterwards — subshells, command substitution,
// shell consumers, expansion-hidden subcommands, and the recursion-depth
// bail-out. Any of these returning a resolvable EffectiveCwd would let the
// caller hash the base repo and consume its marker while the commit actually
// runs in a shell-dependent directory.
func TestAnalyze_AmbiguityPropagation(t *testing.T) {
	t.Setenv("HOME", "/home/tester")

	tests := []struct {
		name string
		cmd  string
	}{
		{
			name: "subshell commit after ambiguous cd",
			cmd:  `cd ~"/x" && (git commit -m x)`,
		},
		{
			name: "command-substitution commit after ambiguous cd",
			cmd:  `cd ~"/x" && echo $(git commit -m x)`,
		},
		{
			name: "shell-consumer commit after ambiguous cd",
			cmd:  `cd ~"/x" && bash -c 'git commit -m x'`,
		},
		{
			name: "eval commit after ambiguous cd",
			cmd:  `cd ~"/x" && eval "git commit -m x"`,
		},
		{
			name: "expansion-hidden subcommand after ambiguous cd",
			cmd:  `cd ~"/x" && git $SUB -m x`,
		},
		{
			name: "recursion-depth bail-out is unresolved",
			cmd:  `( ( ( ( ( ( ( ( git commit -m x ) ) ) ) ) ) ) )`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Analyze(tt.cmd, "/repo")
			if !r.IsGitCommit {
				t.Fatalf("Analyze(%q) IsGitCommit = false, want true", tt.cmd)
			}
			if !r.CwdUnresolved {
				t.Errorf("Analyze(%q) CwdUnresolved = false, want true (EffectiveCwd=%q would be hashed)", tt.cmd, r.EffectiveCwd)
			}
		})
	}
}

// TestAnalyze_HomeMutation checks that a command manipulating HOME makes
// every tilde-led cwd form unresolved: the analyzer expands tildes with the
// hook process's home, but the executing shell would use the mutated value,
// so the two can authorize different repositories.
func TestAnalyze_HomeMutation(t *testing.T) {
	t.Setenv("HOME", "/home/tester")

	tests := []struct {
		name           string
		cmd            string
		wantUnresolved bool
		wantCwd        string
	}{
		{
			name:           "assignment then tilde cd",
			cmd:            `HOME=/tmp/other; cd ~/repo && git commit -m x`,
			wantUnresolved: true,
		},
		{
			name:           "export then tilde cd",
			cmd:            `export HOME=/tmp/other; cd ~/repo && git commit -m x`,
			wantUnresolved: true,
		},
		{
			name:           "unset then tilde cd",
			cmd:            `unset HOME; cd ~/repo && git commit -m x`,
			wantUnresolved: true,
		},
		{
			name:           "assignment then bare-tilde cd",
			cmd:            `HOME=/tmp/other; cd ~ && git commit -m x`,
			wantUnresolved: true,
		},
		{
			name:           "assignment then git -C tilde",
			cmd:            `HOME=/tmp/other; git -C ~/repo commit -m x`,
			wantUnresolved: true,
		},
		{
			name:           "assignment outside inner tilde cd",
			cmd:            `HOME=/tmp/other; bash -c 'cd ~/repo && git commit -m x'`,
			wantUnresolved: true,
		},
		{
			name:           "env -u HOME shell consumer with tilde cd",
			cmd:            `env -u HOME sh -c 'cd ~/repo && git commit -m x'`,
			wantUnresolved: true,
		},
		{
			name:           "env HOME= shell consumer with tilde cd",
			cmd:            `env HOME=/tmp/other sh -c 'cd ~/repo && git commit -m x'`,
			wantUnresolved: true,
		},
		{
			name:    "HOME prefix assignment without tilde stays resolved",
			cmd:     `HOME=/tmp/other git commit -m x`,
			wantCwd: "/repo",
		},
		{
			name:    "HOME= argument to a non-env command stays resolved",
			cmd:     `cd ~/repo && make HOME=/tmp/build && git commit -m x`,
			wantCwd: "/home/tester/repo",
		},
		{
			name:    "HOME= inside a commit message stays resolved",
			cmd:     `cd ~/repo && git commit -m "HOME= handling fix"`,
			wantCwd: "/home/tester/repo",
		},
		{
			name:    "echoed HOME= text stays resolved",
			cmd:     `echo HOME=/tmp; cd ~/repo && git commit -m x`,
			wantCwd: "/home/tester/repo",
		},
		{
			name:    "assignment with absolute cd stays resolved",
			cmd:     `HOME=/tmp/other; cd /abs && git commit -m x`,
			wantCwd: "/abs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Analyze(tt.cmd, "/repo")
			if !r.IsGitCommit {
				t.Fatalf("Analyze(%q) IsGitCommit = false, want true", tt.cmd)
			}
			if r.CwdUnresolved != tt.wantUnresolved {
				t.Errorf("Analyze(%q) CwdUnresolved = %v, want %v", tt.cmd, r.CwdUnresolved, tt.wantUnresolved)
			}
			if !tt.wantUnresolved && r.EffectiveCwd != tt.wantCwd {
				t.Errorf("Analyze(%q) EffectiveCwd = %q, want %q", tt.cmd, r.EffectiveCwd, tt.wantCwd)
			}
		})
	}
}
