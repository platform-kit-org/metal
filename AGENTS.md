# Notes for coding agents

Read [CONTRIBUTING.md](CONTRIBUTING.md) and the develop page
([docs/src/content/docs/project/develop.mdx](docs/src/content/docs/project/develop.mdx))
first. This file adds the rules that a change most often breaks without a
failing test, and the check that now catches each one.

## Commands

The Go module is in `metal/`. Run tasks from the repository root.

| Task | When |
| --- | --- |
| `mise run test` | Each change to Go code. Minutes. |
| `go test -run <Name> ./internal/<pkg>/` in `metal/` | While you work on one test. |
| `mise run envtest` | A change to a controller, a CRD, RBAC or an admission policy. About 5 minutes; run one test with `go test -run`. |
| `mise run lint` | Before each commit: golangci-lint (also gofmt and goimports), the typed checks, shellcheck, govulncheck. |
| `mise run spec` | After a change to a TLA+ specification in `metal/spec`, or to a Go function that one names. Needs a Java runtime. |
| `mise run generate`, then `mise run generate:check` | After a change to API types, kubebuilder markers, `pk-metal` commands or the glossary. |
| `mise run docs:check` and `mise run docs:lint` | After a change to a page. |

Never edit a generated file: `metal/config/crd/`, `metal/config/rbac/role.yaml`,
`docs/src/data/generated/`, `zz_generated.deepcopy.go`. The hook in
`.claude/settings.json` refuses such an edit.

## Rules that a test now holds

Each row is a defect that a review found. Keep the check green; do not change
the check to make a change pass.

| Rule | What went wrong without it | The check |
| --- | --- | --- |
| A decision that a later pass cannot undo reads its inputs through the API reader (`r.reader()`, `APIReader`), not the cache. Mark the function `//pk:uncached`. A function that removes a finalizer has `//pk:uncached`, or `//pk:cached-ok <reason>`. | Two rollouts on one cluster, a second key for a BMC identity, an old password set again, a finalizer released while a claim held the node. | `mise run lint:reads` (`internal/lint`). Each fixed decision is in `mustBeUncached`. |
| A field selector on a list from the API reader names a field that the CRD declares with `// +kubebuilder:selectablefield`. In unit tests, give the fake client the same field with `WithIndex`. | The API server refuses the list, and nothing starts. The fake client accepts any field. | `TestFieldSelectorsAreSelectableFields`, envtest `TestAPIServerFiltersByHostAndCluster`. |
| A change of the code of a Go function that a model names (a `\* go:` line in a TLA+ specification in `metal/spec`, a `// go:` line in `internal/controller/protocol_model_test.go`) also changes the model. | The model proves an older protocol. | `TestSpecSync` (`go test ./spec`, part of `mise run test`). Read the part of the model below the line, change it if the code does something new, run `mise run spec` or `TestProtocolModel`, then record with `go test ./spec -run TestSpecSync/<file> -update`. |
| An alert names only series that the manager serves, and a runbook that exists. | An alert that never fires. | `TestAlertRulesNameServedSeriesAndRunbooks`, `mise run metrics:rules`. |
| A name stamp (mutating policy) and the validating policy of the same field agree. | The API server writes a value that the next check refuses. | envtest `TestNameStampMatchesTheValidatingPolicies`. |
| A runbook step uses only what `pk-metal-oncall` may do. | 403 in the first step of an incident. | envtest `TestOnCallRole` lists the steps. Add a case when a runbook gets a new `kubectl` verb or resource. |
| BMC behavior lives in the simulator (`internal/bmc/sim`) and in the conformance checks (`bmctest`), for each fixture. | The client works on the simulator only. | `bmctest.Run`. Model a vendor quirk only from a record of real hardware. |

## How to prove a fix

1. Write the test first. It must fail on the code without the fix, for the
   reason that the fix removes. Revert the fix once, run the test, and see it
   fail; then restore the fix.
2. A test that passes either way proves nothing. A conformance check that
   waits for a later read-back can hide an error of an earlier step.
3. For a stale cache, give the reconciler a client whose reads answer from an
   older snapshot (`staleLists`, `staleSecrets`, `staleClaims` in
   `internal/controller`), and the real store as `APIReader`.

## Keep the repository consistent

A change is complete when the code, the tests, the docs and the generated
files agree. `.claude/skills/change-checklist` lists what else to change for
each kind of change.

## Writing

- Pages and commit bodies use ASD-STE100 rules: short sentences, one
  instruction for each step, the words in `docs/styles`. Vale checks the pages
  (`mise run docs:lint`); an error fails CI.
- Commits: Conventional Commits, one logical change for each commit, a body
  of short bullets that says what was wrong and what changed.
