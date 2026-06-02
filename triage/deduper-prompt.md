# Deduper: merge six lens reports into a single findings list

You will receive six review reports — three from Codex (`gpt-5.5 high`) and three from Claude (`opus xhigh`). Each report covers one lens: security, architecture, quality, security_xcheck, frontend, test_effectiveness.

Your job has three parts:

1. **Merge duplicates.** When two lenses describe the same underlying defect — same root cause, same file, same fix shape — collapse them into a single finding. Set `lens_sources` to the list of lenses that raised it. Use the most precise file/line citation and the most descriptive `description` and `suggested_fix` text across the contributing findings; preserve the highest severity.

2. **Classify each merged finding as `valid` or `false_positive`** with a code-grounded reason.

3. **Emit structured JSON** matching the provided schema. Do not write anything outside the JSON.

## What is a duplicate

Two findings are duplicates when **all** of:
- They cite the same file (or the same file plus an unspecified-line vs specific-line variant).
- They describe the same underlying defect — the test to apply: would a single fix address both?
- They imply the same suggested change in shape, even if worded differently.

Two findings that share a *file* but describe different issues are NOT duplicates. Don't collapse them.

Two findings that share a *category* (e.g. "missing input validation") but live in different files are NOT duplicates.

## What is a false positive

Be skeptical, but conservative. Label a finding `false_positive` ONLY when you can point to specific code that contradicts it. Acceptable patterns:

- "The reviewer flagged X as unauthenticated, but `auth_middleware.go:42` shows the middleware is applied to this route via the router group at `routes.go:18`."
- "The reviewer claims this is SQL injection, but the query at `db.go:88` uses parameterized binding; the `?` placeholders are not string-interpolated."
- "The reviewer says this hook lacks a dependency, but the value is a stable constant declared outside the component; including it would not change behavior."

**Unacceptable** false_positive reasons (these are vague — if you write them, mark the finding `valid` instead):

- "Probably fine."
- "This might be intentional."
- "The reviewer may have missed context."
- "Could be a known pattern in this codebase."
- "Style / preference issue."
- Anything that doesn't cite specific code.

When in doubt: **default to `valid`**. The conductor verifies every false_positive label before proceeding and will promote weak rejections back to `valid` anyway, so being lax with the label only creates rework.

## Severity

When merging, take the **highest** severity any contributing lens assigned. The dedupe step does not override severity — the conductor will sequence fixes by severity.

## ID assignment

Number findings `F001`, `F002`, ... in the order you emit them. Order by severity descending (critical → high → medium → low), then by file path ascending.

## Output

Emit a single JSON object matching the schema. No prose before or after. No markdown code fences. If the schema validator rejects your output, you will be re-run.

The six lens reports follow.
