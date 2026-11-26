# TLA+ specifications

Each directory holds one protocol of the operator, written in TLA+ and
model-checked with TLC. The concept page
[Protocol model](https://metal.platform-kit.org/concepts/protocol-model/)
explains them.

| Directory | Protocol | Go test of the same invariants |
| --- | --- | --- |
| `claimhost/` | The hand-off of a Host between claims: bind, release, unbind, and the Host phase, with a late cache per kind | `internal/envtest/staleread_claimhost_test.go` |

## Files of a specification

| File | Contents |
| --- | --- |
| `<Name>.tla` | The model: variables, actions, invariants and properties |
| `MC.tla` | The bounds of a run, and `Never*` states that the model must reach |
| `MC.cfg`, `MC_*.cfg` | The TLC configurations; each one must pass |
| `MC_cache.cfg` | A configuration that must fail: it keeps a derivation from before a fix, and proves that the property catches the defect |

## Run

```sh
mise run spec            # or, in metal/: hack/tlc.sh
```

The task needs a Java runtime (11 or later). It downloads the TLA+ tools at a
pinned version and checksum to `metal/bin/tla/`.

## Keep a specification and the code in step

- A `\* go: <package>.<function> <fingerprint>` line names a Go function that
  the operator below it mirrors.
- `go test ./spec` (part of `mise run test`) fails when such a function
  changes or is gone, and when the configurations and the Go test check
  invariants with different names.
- When it fails: read the operator, change it if the code does something new,
  run `mise run spec`, then record the new fingerprints:

  ```sh
  go test ./spec -run TestSpecSync -update
  ```
