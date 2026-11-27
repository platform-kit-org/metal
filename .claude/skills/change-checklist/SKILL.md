---
name: change-checklist
description: What else to change in this repository for each kind of change (API field, RBAC, manager flag, alert, runbook, BMC behavior, admission policy, mirrored function, TLA+ specification), so that code, tests, docs and generated files agree. Use before you commit or open a pull request.
---

# Change checklist

A change is complete when the code, the tests, the docs and the generated
files agree. Find each kind of change that your diff makes, and do each row.
Paths are from the repository root; the Go module is `metal/`.

| You changed | Also change | The check that finds a miss |
| --- | --- | --- |
| A field of an API type (`metal/api/**/*_types.go`) | The comment is the text of the reference page: write it. Validation markers. `mise run generate`. A sample in `metal/config/samples` if a person sets the field. The kind page under `docs/src/content/docs/reference/kinds/`. A breaking change (`mise run crd:diff`): a row in the upgrade notes of `operate/day-2/upgrade-operator.mdx`. | `generate:check`, `config/samples/samples_test.go` (strict decode), `crd:diff` in CI |
| A field selector on a list | `// +kubebuilder:selectablefield:JSONPath=...` on the type, `mise run generate`, `WithIndex` on the fake client of the unit tests. The minimum Kubernetes in `project/versions.mdx` is 1.32. | `TestFieldSelectorsAreSelectableFields`, envtest |
| A `+kubebuilder:rbac` marker | `mise run generate` (role and `rbac.json`). The rights table of the controller page. `reference/security/rbac.mdx` if a shipped role changes. | `generate:check`, envtest `TestRBAC` |
| A shipped role (`metal/config/rbac/*_role.yaml`) | `reference/security/rbac.mdx`, `mise run generate`. A case in envtest `TestOnCallRole` or `TestRBAC`. | envtest |
| A flag of the manager (`metal/cmd/manager/main.go`) | Validation and a refusal case in `main_test.go`. `Options` in `internal/controller/setup.go`. A row in `reference/controllers/manager.mdx`. | `cmd/manager` tests |
| A `pk-metal` command or flag | `mise run generate` (the CLI reference data). The how-to that uses it. | `generate:check` |
| An alert or a metric (`metal/config/prometheus`) | A promtool case in `internal/metrics/testdata/rules_test.yaml`. A runbook under `operate/runbooks/`, its row in `runbooks/index.mdx` and in `operate/day-2/monitor.mdx`. | `mise run metrics:rules`, `TestAlertRulesNameServedSeriesAndRunbooks` |
| A runbook step with `kubectl` | The on-call role allows it, and `TestOnCallRole` has the case. | envtest |
| BMC behavior | The simulator (`metal/internal/bmc/sim`) for each fixture, a protocol knob in `fixture.go` if a vendor differs, a conformance check in `bmctest`. Rows in `reference/hardware.mdx` and `reference/lab/simulated-bmc.mdx`. | `bmctest.Run` for each fixture |
| An admission policy (`metal/config/policy`, `metal/config/name-stamp`) | The envtest policy tests. `reference/security/admission-policies.mdx`. A stamp and the validating policy of the same field must agree. | envtest `TestNameStampMatchesTheValidatingPolicies` |
| A function that the protocol model mirrors (see `mirrored` in `metal/spec/mirror_test.go`) | The pass of the model that mirrors it in `protocol_model_test.go`. Run `TestProtocolModel`, then record the new hash. | `TestModelMirrorsTheCode` |
| A function that a TLA+ specification names (a `\* go:` line in `metal/spec/*/*.tla`) | The operator that names it, if the code does something new. Run `mise run spec`, then `go test ./spec -run TestSpecSync -update`. `concepts/protocol-model.mdx` if the steps change. | `TestSpecSync` |
| A decision that a later pass cannot undo | Read its inputs through the API reader, mark the function `//pk:uncached`, add it to `mustBeUncached`, write a stale-cache test. A row in "Reads from the API server" of `concepts/architecture.mdx`. See the skill `stale-cache-fix`. | `mise run lint:reads` |
| A new action of a HostOperation | The table "Add an action" of `project/develop.mdx`: eleven places, three of them with no check. | Partly unit tests |
| A controller | Its page under `reference/controllers/`: watches, loop, writes, failure modes, tests. | `docs:check` for the links only |

## Before you commit

Run the checks in "Before you commit" of
`docs/src/content/docs/project/develop.mdx`.
