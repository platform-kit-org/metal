# Contributing to platform-kit

Read the [project status](README.md#status) first, and open an issue before
you start a large change. The code is under the Apache License 2.0
([LICENSE](LICENSE)). A contribution is under the same license.

The [documentation site](https://metal.platform-kit.org/) explains
the design. Start with the
[architecture page](https://metal.platform-kit.org/concepts/architecture/).
The [page for people who change the code](https://metal.platform-kit.org/project/develop/)
has the rules. This file adds the rules for commits.

## Set up

1. Install [mise](https://mise.jdx.dev/).
2. Run `mise trust` and then `mise install` in the repository root.
3. Run `mise tasks` to see every task.

mise pins Go, kubectl, kind, talosctl, golangci-lint, jq, yq, promtool, Node
and Vale, each to its minor version.
Docker, QEMU, OVMF and shellcheck are system packages.

Development happens on the `main` branch. Start your branch from it.

## Where the rules are

- [The test ladder](https://metal.platform-kit.org/project/develop/#the-test-ladder):
  the test levels, and where a test goes.
- [Before you commit](https://metal.platform-kit.org/project/develop/#before-you-commit):
  the checks, and the generated files.
- [The invariants](https://metal.platform-kit.org/project/develop/#the-invariants):
  the rules that each change must keep.
- [Write for this site](https://metal.platform-kit.org/project/develop/#write-for-this-site):
  the pages and their writing rules.
- [AGENTS.md](AGENTS.md): the rules that a test now holds.

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/):
`type(scope): subject`.

- Types in use: `feat`, `fix`, `docs`, `test`, `ci`, `chore`, `build`,
  `refactor`, `perf`.
- The scope is the part you changed: `controller`, `bmc`, `talos`, `pk-metal`,
  `api`, `lab`, `site` (for the documentation site), and so on.
- Write the subject in the imperative, in lower case, without a full stop.
- Write the body as short bullets that say what changed and why.
- Make one logical change in each commit. Include its tests and its generated
  files.

## Before you open a pull request

Run the checks in
[Before you commit](https://metal.platform-kit.org/project/develop/#before-you-commit).

## Security

Do not open a public issue for a vulnerability. Read [SECURITY.md](SECURITY.md).
