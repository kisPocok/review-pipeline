// Package detect analyzes a shell command for a real `git commit` invocation,
// resolving the effective cwd and any --git-dir / --work-tree globals so the
// caller can compute a matching `git write-tree` hash.
package detect

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Result is the outcome of analyzing a shell command.
type Result struct {
	// IsGitCommit reports whether the command invokes a `git commit` that
	// would actually create a commit (not --dry-run / --help / etc.).
	IsGitCommit bool
	// EffectiveCwd is the working directory where the commit will run,
	// after walking any `cd` or `git -C` calls. Only meaningful when
	// IsGitCommit is true.
	EffectiveCwd string
	// GitGlobals are git global args (--git-dir, --work-tree) captured from
	// the commit segment, ordered for splicing before a `write-tree` call.
	GitGlobals []string
}

// Analyze parses cmd as a shell command and reports a Result.
//
// baseCwd is the cwd in which cmd would be evaluated; it serves as the
// starting point before any `cd` or `git -C` is applied. Relative paths in
// `cd` are resolved against the current cwd.
func Analyze(cmd, baseCwd string) Result {
	return analyze(cmd, baseCwd, 0)
}

const maxRecursion = 4

var (
	shellConsumers = map[string]bool{
		"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true,
		"eval": true, "source": true, "exec": true, ".": true,
	}
	// Transparent precommands are commands that pass through their argv to
	// run the next command in the same shell context.
	transparentPrecommands = map[string]bool{
		"env": true, "time": true, "command": true,
		"exec": true, "nice": true, "ionice": true, "stdbuf": true,
	}
	globalValueOpts = map[string]bool{
		"-C": true, "-c": true,
		"--git-dir": true, "--work-tree": true,
		"--namespace": true, "--super-prefix": true, "--config-env": true,
	}
	globalInfoOpts = map[string]bool{
		"--help": true, "-h": true, "--version": true,
		"--html-path": true, "--man-path": true,
		"--info-path": true, "--exec-path": true,
	}
	commitValueOpts = map[string]bool{
		"-m": true, "--message": true,
		"-F": true, "--file": true,
		"-c": true, "-C": true,
		"--cleanup": true, "--author": true, "--date": true,
		"--reuse-message": true, "--reedit-message": true,
		"--fixup": true, "--squash": true,
		"--template": true, "--pathspec-from-file": true,
		"--trailer":          true,
		"-u":                 true,
		"--untracked-files":  true,
	}
	commitOptionalValueOpts = map[string]bool{
		"-S": true, "--gpg-sign": true,
	}
	commitSkipFlags = map[string]bool{
		"--dry-run": true, "--help": true, "-h": true,
	}
)

func analyze(cmd, baseCwd string, depth int) Result {
	if depth > maxRecursion {
		// Fail-closed on pathological nesting.
		return Result{IsGitCommit: true, EffectiveCwd: baseCwd}
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return Result{}
	}

	cwd := baseCwd
	for _, stmt := range flattenSameLevel(file.Stmts) {
		// First, check for a real git commit nested inside CmdSubst / Subshell.
		// Those don't update the outer cwd but DO count for detection.
		if r := findNested(stmt, cwd, depth); r.IsGitCommit {
			return r
		}

		call := topLevelCall(stmt)
		if call == nil {
			continue
		}
		args := wordsToStrings(call.Args)
		if len(args) == 0 {
			continue
		}
		args = stripTransparent(args)
		if len(args) == 0 {
			continue
		}
		// Offset of args[0] within call.Args after stripTransparent trimmed
		// the front, so positions map back to their syntax.Word.
		wordOffset := len(call.Args) - len(args)

		head := filepath.Base(args[0])

		// Shell consumers: recurse into their script argument.
		if shellConsumers[head] {
			if r := recurseShellConsumer(head, args, stmt, cwd, depth); r.IsGitCommit {
				return r
			}
			continue
		}

		// Heredoc body: recurse only when consumer is a shell.
		// (Non-shell consumer was already filtered above.)
		// For non-shell-consumer commands with heredocs, do nothing.

		if head == "cd" && len(args) >= 2 {
			cwd = resolveCwd(args[1], cwd)
			continue
		}

		if head != "git" {
			continue
		}

		subIdx, hasInfo, gitC, globals := walkGitGlobals(args)
		if hasInfo {
			continue
		}
		// Expansions ($VAR, $(…)) resolve to empty text in wordsToStrings, so
		// a subcommand hidden behind one is invisible to the checks below.
		// Like the maxRecursion case, that uncertainty fails closed.
		if expansionBeforeSubcommand(call.Args, wordOffset, subIdx) {
			return Result{IsGitCommit: true, EffectiveCwd: cwd}
		}
		if subIdx >= len(args) || args[subIdx] != "commit" {
			continue
		}
		if !commitIsReal(args[subIdx+1:]) {
			continue
		}

		effective := cwd
		if gitC != "" {
			effective = resolveCwd(gitC, cwd)
		}
		return Result{
			IsGitCommit:  true,
			EffectiveCwd: effective,
			GitGlobals:   globals,
		}
	}
	return Result{}
}

// flattenSameLevel expands BinaryCmd nodes (&&, ||) so their two operands
// surface as sibling statements at the same logical execution level. `;` and
// `\n` already produce sibling Stmts in mvdan/sh's parse tree.
func flattenSameLevel(stmts []*syntax.Stmt) []*syntax.Stmt {
	out := make([]*syntax.Stmt, 0, len(stmts))
	for _, s := range stmts {
		switch c := s.Cmd.(type) {
		case *syntax.BinaryCmd:
			out = append(out, flattenSameLevel([]*syntax.Stmt{c.X, c.Y})...)
		default:
			out = append(out, s)
		}
	}
	return out
}

// topLevelCall returns the *CallExpr inside stmt if stmt is a simple command
// (possibly via a single Time wrapper). Returns nil for compound commands
// (subshells, blocks, etc.).
func topLevelCall(stmt *syntax.Stmt) *syntax.CallExpr {
	switch c := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		return c
	case *syntax.TimeClause:
		if c.Stmt != nil {
			return topLevelCall(c.Stmt)
		}
	}
	return nil
}

// findNested walks stmt's tree and looks for a real git commit inside
// CmdSubst or Subshell nodes. Detection-only — does not propagate cwd.
func findNested(stmt *syntax.Stmt, cwd string, depth int) Result {
	var found Result
	syntax.Walk(stmt, func(n syntax.Node) bool {
		if found.IsGitCommit {
			return false
		}
		switch x := n.(type) {
		case *syntax.CmdSubst:
			r := analyzeStmts(x.Stmts, cwd, depth+1)
			if r.IsGitCommit {
				r.EffectiveCwd = cwd
				r.GitGlobals = nil
				found = r
			}
			return false
		case *syntax.Subshell:
			r := analyzeStmts(x.Stmts, cwd, depth+1)
			if r.IsGitCommit {
				r.EffectiveCwd = cwd
				r.GitGlobals = nil
				found = r
			}
			return false
		case *syntax.ProcSubst:
			r := analyzeStmts(x.Stmts, cwd, depth+1)
			if r.IsGitCommit {
				r.EffectiveCwd = cwd
				r.GitGlobals = nil
				found = r
			}
			return false
		}
		return true
	})
	return found
}

// analyzeStmts re-runs the analyzer on an already-parsed list of statements.
func analyzeStmts(stmts []*syntax.Stmt, baseCwd string, depth int) Result {
	if depth > maxRecursion {
		return Result{IsGitCommit: true, EffectiveCwd: baseCwd}
	}
	// Re-print to source so we can use the same analyze() path uniformly.
	var b strings.Builder
	p := syntax.NewPrinter()
	for _, s := range stmts {
		if err := p.Print(&b, s); err != nil {
			return Result{}
		}
		b.WriteString("\n")
	}
	return analyze(b.String(), baseCwd, depth)
}

// recurseShellConsumer handles bash/sh/zsh/eval/source/exec/. by extracting
// the script argument (after -c or via heredoc) and re-analyzing it.
func recurseShellConsumer(head string, args []string, stmt *syntax.Stmt, cwd string, depth int) Result {
	// -c <script>
	for i := 1; i < len(args)-1; i++ {
		if args[i] == "-c" {
			r := analyze(args[i+1], cwd, depth+1)
			if r.IsGitCommit {
				r.EffectiveCwd = cwd
				r.GitGlobals = nil
				return r
			}
		}
	}
	// eval joins all remaining args as one shell script.
	if head == "eval" && len(args) > 1 {
		joined := strings.Join(args[1:], " ")
		r := analyze(joined, cwd, depth+1)
		if r.IsGitCommit {
			r.EffectiveCwd = cwd
			r.GitGlobals = nil
			return r
		}
	}
	// Heredoc body, if any redirect on this stmt is Hdoc.
	for _, rd := range stmt.Redirs {
		if rd.Op != syntax.Hdoc && rd.Op != syntax.DashHdoc {
			continue
		}
		if rd.Hdoc == nil {
			continue
		}
		body := wordLit(rd.Hdoc)
		r := analyze(body, cwd, depth+1)
		if r.IsGitCommit {
			r.EffectiveCwd = cwd
			r.GitGlobals = nil
			return r
		}
	}
	return Result{}
}

// stripTransparent walks past env-style precommands (env VAR=x, time -p, command,
// nice -n 5, etc.) to surface the actual command being invoked.
func stripTransparent(args []string) []string {
	for len(args) > 0 {
		head := filepath.Base(args[0])
		if !transparentPrecommands[head] {
			break
		}
		args = args[1:]
		switch head {
		case "env":
			// env may be followed by options (-i, -u VAR) and VAR=value pairs.
			for len(args) > 0 {
				a := args[0]
				if strings.HasPrefix(a, "-") {
					// crude: consume the flag; if it's -u, consume its argument too.
					args = args[1:]
					if a == "-u" && len(args) > 0 {
						args = args[1:]
					}
					continue
				}
				if isEnvAssignment(a) {
					args = args[1:]
					continue
				}
				break
			}
		case "time":
			if len(args) > 0 && args[0] == "-p" {
				args = args[1:]
			}
		case "command", "exec", "nice", "ionice", "stdbuf":
			// consume any leading -X flags
			for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "--" {
				args = args[1:]
			}
			if len(args) > 0 && args[0] == "--" {
				args = args[1:]
			}
		}
	}
	return args
}

func isEnvAssignment(s string) bool {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return false
	}
	name := s[:eq]
	if name == "" {
		return false
	}
	if !isAlpha(name[0]) && name[0] != '_' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !isAlpha(c) && !isDigit(c) && c != '_' {
			return false
		}
	}
	return true
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// walkGitGlobals scans args (where args[0]=="git") and returns:
//   - subIdx: index of the subcommand (or len(args) if none)
//   - hasInfo: true if a global like --help/--version was used (info path)
//   - gitC: the value of the last `-C <path>` global, or "" if none
//   - globals: --git-dir / --work-tree forms in order for replay
func walkGitGlobals(args []string) (subIdx int, hasInfo bool, gitC string, globals []string) {
	i := 1
	for i < len(args) {
		t := args[i]
		if !strings.HasPrefix(t, "-") {
			break
		}
		if globalInfoOpts[t] {
			hasInfo = true
			i++
			continue
		}
		// --exec-path=val is value-setting; not info. The bare --exec-path form
		// is handled by globalInfoOpts above.
		if strings.HasPrefix(t, "--exec-path=") {
			i++
			continue
		}
		if globalValueOpts[t] {
			if t == "-C" && i+1 < len(args) {
				gitC = args[i+1]
			}
			if t == "--git-dir" && i+1 < len(args) {
				globals = append(globals, "--git-dir", args[i+1])
			}
			if t == "--work-tree" && i+1 < len(args) {
				globals = append(globals, "--work-tree", args[i+1])
			}
			i += 2
			continue
		}
		if strings.HasPrefix(t, "--git-dir=") || strings.HasPrefix(t, "--work-tree=") {
			globals = append(globals, t)
		}
		i++
	}
	return i, hasInfo, gitC, globals
}

// commitIsReal returns false if any commit option is --dry-run / --help / -h
// at the position where it would actually be a flag (not the argument of a
// preceding value-taking flag like -m).
func commitIsReal(args []string) bool {
	j := 0
	for j < len(args) {
		a := args[j]
		if commitValueOpts[a] {
			j += 2
			continue
		}
		if commitOptionalValueOpts[a] {
			// Take the next token only if it doesn't look like a flag.
			if j+1 < len(args) && !strings.HasPrefix(args[j+1], "-") {
				j += 2
				continue
			}
			j++
			continue
		}
		if commitSkipFlags[a] {
			return false
		}
		j++
	}
	return true
}

// resolveCwd resolves target against base. Absolute targets are returned
// cleaned; relative targets are joined onto base.
func resolveCwd(target, base string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(base, target))
}

// wordsToStrings extracts the literal text of each *syntax.Word.
func wordsToStrings(words []*syntax.Word) []string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		out = append(out, wordLit(w))
	}
	return out
}

// expansionBeforeSubcommand reports whether any git arg from position 1 up to
// and including the subcommand position contains an unresolvable expansion.
// offset maps stripped-arg positions back into words (see stripTransparent).
func expansionBeforeSubcommand(words []*syntax.Word, offset, subIdx int) bool {
	for i := 1; i <= subIdx && offset+i < len(words); i++ {
		if wordHasExpansion(words[offset+i]) {
			return true
		}
	}
	return false
}

// wordHasExpansion reports whether w contains any part that wordLit cannot
// resolve to literal text (ParamExp, CmdSubst, ArithmExp, etc.).
func wordHasExpansion(w *syntax.Word) bool {
	if w == nil {
		return false
	}
	for _, p := range w.Parts {
		switch x := p.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
		case *syntax.DblQuoted:
			for _, pp := range x.Parts {
				if _, ok := pp.(*syntax.Lit); !ok {
					return true
				}
			}
		default:
			return true
		}
	}
	return false
}

// wordLit returns the literal content of a Word, treating quoted parts as
// their unquoted text. Variable expansions and command substitutions resolve
// to empty.
func wordLit(w *syntax.Word) string {
	if w == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range w.Parts {
		switch x := p.(type) {
		case *syntax.Lit:
			b.WriteString(x.Value)
		case *syntax.SglQuoted:
			b.WriteString(x.Value)
		case *syntax.DblQuoted:
			for _, pp := range x.Parts {
				if lit, ok := pp.(*syntax.Lit); ok {
					b.WriteString(lit.Value)
				}
			}
		}
	}
	return b.String()
}
