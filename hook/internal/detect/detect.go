// Package detect analyzes a shell command for a real `git commit` invocation,
// resolving the effective cwd and any --git-dir / --work-tree globals so the
// caller can compute a matching `git write-tree` hash.
package detect

import (
	"os"
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
	// CwdUnresolved reports that the commit's directory is unknowable: a
	// cwd-affecting value (cd target, git -C, --git-dir/--work-tree) carries
	// text the shell computes at run time (expansions, globs, dollar-quotes)
	// or a form whose meaning depends on the executing shell (mixed-quoted
	// tilde, ~user, any tilde once the command manipulates HOME, option-led
	// cd), shell expansions hide the git subcommand or an option word, or
	// nesting exceeded the analysis depth. The caller must block WITHOUT
	// computing or consuming any tree hash: a literal fallback is itself a
	// resolution, and a decoy repo at the literal path could be used to
	// authorize the wrong tree.
	CwdUnresolved bool
}

// Analyze parses cmd as a shell command and reports a Result.
//
// baseCwd is the cwd in which cmd would be evaluated; it serves as the
// starting point before any `cd` or `git -C` is applied. Relative paths in
// `cd` are resolved against the current cwd.
func Analyze(cmd, baseCwd string) Result {
	return analyze(cmd, baseCwd, 0, false)
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
		"--trailer":         true,
		"-u":                true,
		"--untracked-files": true,
	}
	commitOptionalValueOpts = map[string]bool{
		"-S": true, "--gpg-sign": true,
	}
	commitSkipFlags = map[string]bool{
		"--dry-run": true, "--help": true, "-h": true,
	}
)

// analyze walks cmd's statements for a real git commit. homeMutated carries
// a HOME manipulation from an enclosing command into nested scripts, whose
// own text can't reveal it; any mutation makes every tilde expansion
// shell-dependent.
func analyze(cmd, baseCwd string, depth int, homeMutated bool) Result {
	if depth > maxRecursion {
		// Fail-closed on pathological nesting: the commit's directory is
		// unknowable, so no tree may be hashed or marker consumed.
		return Result{IsGitCommit: true, CwdUnresolved: true}
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return Result{}
	}
	if !homeMutated {
		homeMutated = mutatesHome(file)
	}

	cwd := baseCwd
	cwdAmbiguous := false
	for _, stmt := range flattenSameLevel(file.Stmts) {
		// First, check for a real git commit nested inside CmdSubst / Subshell.
		// Those don't update the outer cwd but DO count for detection.
		if r := findNested(stmt, cwd, depth, homeMutated); r.IsGitCommit {
			if cwdAmbiguous {
				r.CwdUnresolved = true
			}
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
			if r := recurseShellConsumer(head, args, stmt, cwd, depth, homeMutated); r.IsGitCommit {
				if cwdAmbiguous {
					r.CwdUnresolved = true
				}
				return r
			}
			continue
		}

		// Heredoc body: recurse only when consumer is a shell.
		// (Non-shell consumer was already filtered above.)
		// For non-shell-consumer commands with heredocs, do nothing.

		if head == "cd" {
			cdArgs := args[1:]
			cdWords := call.Args[wordOffset+1:]
			// Only a pure-literal `--` is the option terminator; wordLit
			// collapses expansions, so a fused `--$X` also reads as "--" and
			// must keep flowing into the checks below.
			if len(cdArgs) > 0 && cdArgs[0] == "--" && !wordHasExpansion(cdWords[0]) {
				cdArgs = cdArgs[1:]
				cdWords = cdWords[1:]
			}
			if len(cdArgs) == 0 {
				// Bare cd targets HOME. A relative HOME is chdir'd from the
				// shell's cwd but would be resolved against the hook
				// process's directory, so only an absolute one resolves.
				home, err := os.UserHomeDir()
				if !homeMutated && err == nil && filepath.IsAbs(home) {
					cwdAmbiguous = false
					cwd = home
				} else {
					cwdAmbiguous = true
				}
				continue
			}
			// Option-led cd (-P, -L, `cd -` OLDPWD, …) isn't modeled: the
			// word the analyzer would take as the target is not the target.
			if strings.HasPrefix(cdArgs[0], "-") {
				cwdAmbiguous = true
				continue
			}
			target := cdWords[0]
			// An expansion or glob in the target resolves to different text
			// here than in the shell — a resolved fallback would hash the
			// wrong repository (a decoy can sit at the literal name).
			if tildeUnresolved(target) || wordHasExpansion(target) ||
				litHasGlob(target) || (homeMutated && tildeLed(target)) {
				cwdAmbiguous = true
			} else {
				if filepath.IsAbs(cdArgs[0]) {
					cwdAmbiguous = false
				}
				cwd = resolveCwd(cdArgs[0], cwd)
			}
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
		// a subcommand hidden behind one is invisible to the checks below —
		// and the hidden words can carry -C/--git-dir redirections, so the
		// commit's directory is unknowable, not merely the subcommand.
		if expansionHidesSubcommand(call.Args, wordOffset, subIdx) {
			return Result{IsGitCommit: true, EffectiveCwd: cwd, CwdUnresolved: true}
		}
		if subIdx >= len(args) || args[subIdx] != "commit" {
			continue
		}
		if !commitIsReal(args[subIdx+1:]) {
			continue
		}

		unresolved := cwdAmbiguous
		if gitC != "" && filepath.IsAbs(gitC) {
			// An absolute -C overrides any ambiguity accumulated via cd.
			unresolved = false
		}
		if gitValueUnresolved(call.Args, wordOffset, args, subIdx, homeMutated) {
			unresolved = true
		}
		if unresolved {
			return Result{IsGitCommit: true, EffectiveCwd: cwd, CwdUnresolved: true}
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
func findNested(stmt *syntax.Stmt, cwd string, depth int, homeMutated bool) Result {
	var found Result
	syntax.Walk(stmt, func(n syntax.Node) bool {
		if found.IsGitCommit {
			return false
		}
		switch x := n.(type) {
		case *syntax.CmdSubst:
			r := analyzeStmts(x.Stmts, cwd, depth+1, homeMutated)
			if r.IsGitCommit {
				r.EffectiveCwd = cwd
				r.GitGlobals = nil
				found = r
			}
			return false
		case *syntax.Subshell:
			r := analyzeStmts(x.Stmts, cwd, depth+1, homeMutated)
			if r.IsGitCommit {
				r.EffectiveCwd = cwd
				r.GitGlobals = nil
				found = r
			}
			return false
		case *syntax.ProcSubst:
			r := analyzeStmts(x.Stmts, cwd, depth+1, homeMutated)
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
func analyzeStmts(stmts []*syntax.Stmt, baseCwd string, depth int, homeMutated bool) Result {
	if depth > maxRecursion {
		return Result{IsGitCommit: true, CwdUnresolved: true}
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
	return analyze(b.String(), baseCwd, depth, homeMutated)
}

// recurseShellConsumer handles bash/sh/zsh/eval/source/exec/. by extracting
// the script argument (after -c or via heredoc) and re-analyzing it.
func recurseShellConsumer(head string, args []string, stmt *syntax.Stmt, cwd string, depth int, homeMutated bool) Result {
	// -c <script>
	for i := 1; i < len(args)-1; i++ {
		if args[i] == "-c" {
			r := analyze(args[i+1], cwd, depth+1, homeMutated)
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
		r := analyze(joined, cwd, depth+1, homeMutated)
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
		r := analyze(body, cwd, depth+1, homeMutated)
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

// wordsToStrings extracts the literal text of each *syntax.Word, applying
// tilde expansion the way the shell does for argv words.
func wordsToStrings(words []*syntax.Word) []string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		out = append(out, tildeExpand(wordLit(w), w))
	}
	return out
}

// tildeUnresolved reports whether w begins with an unquoted literal tilde
// whose expansion depends on the executing shell: mixed-quoting forms like
// ~"/x" (bash keeps literal, zsh expands) and ~user forms (bash keeps
// literal for unknown users, zsh expands or errors). Clean forms — a bare ~
// as the whole word, or a ~/ prefix carried in the first literal part — are
// resolved identically by every shell and are not flagged; fully quoted
// tildes stay literal everywhere and are not flagged either.
func tildeUnresolved(w *syntax.Word) bool {
	if w == nil || len(w.Parts) == 0 {
		return false
	}
	first, ok := w.Parts[0].(*syntax.Lit)
	if !ok || !strings.HasPrefix(first.Value, "~") {
		return false
	}
	if first.Value == "~" && len(w.Parts) == 1 {
		return false
	}
	return !strings.HasPrefix(first.Value, "~/")
}

// gitValueUnresolved reports whether any cwd-affecting git global value
// (-C, --git-dir, --work-tree, and their --opt=value forms) before the
// subcommand is unknowable at analysis time: a shell-dependent tilde form,
// any expansion (the shell substitutes real text where the analyzer sees
// empty), a glob the shell may expand to a different path, or — with
// homeMutated — any tilde-led value, since the analyzer's expansion and the
// shell's disagree.
func gitValueUnresolved(words []*syntax.Word, offset int, args []string, subIdx int, homeMutated bool) bool {
	for i := 1; i < subIdx && i < len(args); i++ {
		switch args[i] {
		case "-C", "--git-dir", "--work-tree":
			if wi := offset + i + 1; wi < len(words) {
				if tildeUnresolved(words[wi]) || wordHasExpansion(words[wi]) ||
					litHasGlob(words[wi]) || (homeMutated && tildeLed(words[wi])) {
					return true
				}
			}
		}
		// Equals-joined forms carry their value inside the option word; an
		// expansion there splices real path text into the literal the
		// analyzer records for write-tree replay.
		if strings.HasPrefix(args[i], "--git-dir=") || strings.HasPrefix(args[i], "--work-tree=") {
			if wi := offset + i; wi < len(words) &&
				(wordHasExpansion(words[wi]) || litHasGlob(words[wi])) {
				return true
			}
		}
	}
	return false
}

// litHasGlob reports whether w carries glob or brace metacharacters in its
// unquoted literal parts — the shell may expand them to a different path
// than the literal text the analyzer resolves, and a decoy repository can
// sit at the literal name.
func litHasGlob(w *syntax.Word) bool {
	if w == nil {
		return false
	}
	for _, p := range w.Parts {
		if lit, ok := p.(*syntax.Lit); ok && strings.ContainsAny(lit.Value, "*?[{") {
			return true
		}
	}
	return false
}

// tildeLed reports whether w starts with an unquoted literal tilde of any
// form — the words tildeExpand may act on or tildeUnresolved may flag.
func tildeLed(w *syntax.Word) bool {
	if w == nil || len(w.Parts) == 0 {
		return false
	}
	first, ok := w.Parts[0].(*syntax.Lit)
	return ok && strings.HasPrefix(first.Value, "~")
}

// mutatesHome reports whether the parsed command manipulates HOME anywhere:
// an assignment (standalone, prefix, export/declare), `unset HOME`, an
// `env`-style `HOME=` word, or env flags that drop it (-i, -u HOME). Scope
// and ordering are not modeled — any mutation poisons the whole command,
// which can only over-block (fail closed).
func mutatesHome(file *syntax.File) bool {
	found := false
	syntax.Walk(file, func(n syntax.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *syntax.Assign:
			if x.Name != nil && x.Name.Value == "HOME" {
				found = true
			}
		case *syntax.CallExpr:
			lits := make([]string, 0, len(x.Args))
			for _, w := range x.Args {
				lits = append(lits, wordLit(w))
			}
			if len(lits) == 0 {
				break
			}
			head := filepath.Base(lits[0])
			for i := 1; i < len(lits); i++ {
				a := lits[i]
				if head == "unset" && a == "HOME" {
					found = true
					break
				}
				if head == "env" && (a == "-i" ||
					strings.HasPrefix(a, "HOME=") ||
					(a == "-u" && i+1 < len(lits) && lits[i+1] == "HOME")) {
					found = true
					break
				}
			}
		}
		return true
	})
	return found
}

// tildeExpand expands a leading unquoted tilde in lit to the user's home
// directory, mirroring the shell's tilde expansion of argv words. Expansion
// applies only to the unambiguous forms every shell agrees on: a bare `~`
// that is the entire word, or a `~/` prefix carried unquoted in the word's
// first literal part. Mixed-quoting forms like `~"/x"` diverge between
// shells (bash keeps them literal, zsh expands), so they stay literal —
// as do quoted tildes and ~user/~+/~- forms. Unexpanded values make the
// caller's write-tree fail and the hook fail closed.
func tildeExpand(lit string, w *syntax.Word) string {
	if w == nil || len(w.Parts) == 0 {
		return lit
	}
	first, ok := w.Parts[0].(*syntax.Lit)
	if !ok {
		return lit
	}
	expand := false
	switch {
	case lit == "~":
		expand = len(w.Parts) == 1 && first.Value == "~"
	case strings.HasPrefix(lit, "~/"):
		expand = strings.HasPrefix(first.Value, "~/")
	}
	if !expand {
		return lit
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return lit
	}
	return home + lit[1:]
}

// expansionHidesSubcommand reports whether shell expansions make the git
// subcommand unknowable: the word at the subcommand position contains any
// expansion (its value could be "commit"), an earlier arg carries an
// expansion that can yield multiple argv words and shift a hidden commit
// into subcommand position, or a dash-led option word is not fully literal —
// its literal text classified argv structure (which words consume values),
// so an expansion attached to it (--git-dir"$X") can carry the value at run
// time and shift where the subcommand lands. Quoted scalar expansions in
// value positions before a literal subcommand are safe — they always remain
// a single word that is consumed as a value regardless of content.
// offset maps stripped-arg positions back into words (see stripTransparent).
func expansionHidesSubcommand(words []*syntax.Word, offset, subIdx int) bool {
	if i := offset + subIdx; i < len(words) && wordHasExpansion(words[i]) {
		return true
	}
	for i := 1; i < subIdx && offset+i < len(words); i++ {
		w := words[offset+i]
		if wordHasSplittingExpansion(w) {
			return true
		}
		if strings.HasPrefix(wordLit(w), "-") && wordHasExpansion(w) {
			return true
		}
	}
	return false
}

// wordHasSplittingExpansion reports whether w contains an expansion that can
// yield more than one argv word: any unquoted expansion (IFS splitting), or a
// quoted expansion that splits despite the quotes — "$@" and a literal [@]
// subscript ("${arr[@]}") expand to one word per element, and indirection
// (${!x}) can alias an [@] form, unresolvable statically. Quoted scalar
// expansions ("$VAR", "${arr[0]}", "${arr[*]}", "$(cmd)") always remain a
// single word and are safe.
func wordHasSplittingExpansion(w *syntax.Word) bool {
	if w == nil {
		return false
	}
	for _, p := range w.Parts {
		switch x := p.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
		case *syntax.DblQuoted:
			for _, pp := range x.Parts {
				pe, ok := pp.(*syntax.ParamExp)
				if !ok {
					continue
				}
				if pe.Excl || pe.Param == nil || pe.Param.Value == "@" {
					return true
				}
				if iw, ok := pe.Index.(*syntax.Word); ok && wordLit(iw) == "@" {
					return true
				}
			}
		default:
			return true
		}
	}
	return false
}

// wordHasExpansion reports whether w contains any part that wordLit cannot
// resolve to literal text: ParamExp, CmdSubst, ArithmExp, etc., and the
// dollar-quote forms — $'…' evaluates escape sequences and $"…" is
// locale-translated, so the shell's text differs from the raw value the
// analyzer sees.
func wordHasExpansion(w *syntax.Word) bool {
	if w == nil {
		return false
	}
	for _, p := range w.Parts {
		switch x := p.(type) {
		case *syntax.Lit:
		case *syntax.SglQuoted:
			if x.Dollar {
				return true
			}
		case *syntax.DblQuoted:
			if x.Dollar {
				return true
			}
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
