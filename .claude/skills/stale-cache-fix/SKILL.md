---
name: stale-cache-fix
description: Find and fix a decision in a pk-metal controller that acts on a stale informer cache, with a test that fails without the fix. Use when a reconciler reads one kind and then does something that stays to another (a finalizer, a start, a create, a password).
---

# Fix a stale-cache decision

The manager reads through an informer cache, and the cache can be late. Most
decisions are safe on a late read: the write carries the resourceVersion of
what was read, or the next pass corrects it. A decision is not safe when the
pass reads one kind and then acts on another, and the act stays:

- a finalizer comes off, and the object is gone;
- an operation or a rollout starts, and the machine moves;
- a key, a password or an object is written that a later pass cannot take
  back.

## Find one

1. Look for `controllerutil.RemoveFinalizer`, a start of a step, a `Create`,
   and a write to a BMC.
2. List the reads that the decision depends on, in the same function and in
   the caller. A read of another kind than the one written is a candidate.
3. Ask: if this read shows the world of one second ago, what goes wrong? The
   protocol model (`TestProtocolModel`) explores this for the claim and the
   executor; other lanes need the question by hand.

## Fix it

1. Move the reads into a small function that reads through the API reader:
   `r.reader()` or `r.APIReader` (a `client.Reader`). Wire `APIReader` in
   `SetupWithManager` from `mgr.GetAPIReader()` if the reconciler has none.
2. Mark that function:

   ```go
   // liveHosts lists the hosts from the API server. <why the cache is not enough>
   //
   //pk:uncached
   func (r *ProtectionReconciler) liveHosts(ctx context.Context) ([]metalv1alpha1.Host, error) {
   ```

3. Add `Recv.method` to `mustBeUncached` in `metal/internal/lint/uncached_test.go`.
4. A function that removes a finalizer has `//pk:uncached`, or
   `//pk:cached-ok <why its cached reads are safe>`.
5. Pay for the extra read only on the path that decides, not on each pass.
6. A list with a field selector through the API reader needs a selectable
   field on the CRD; see the skill `change-checklist`.

## Prove it

1. Build the reconciler with a client whose reads answer from an older
   snapshot, and the real store as `APIReader`.
   `internal/controller/stalecache_test.go` has these clients: `staleGet`
   and `staleList` change what a read of one kind returns, and `staleLists`
   answers all lists from a second fake client.
2. Assert the safe outcome. Then set `APIReader` to nil, or revert the fix,
   and see the test fail. A test that passes either way proves nothing.
3. Run `mise run lint:reads` and `go test ./internal/controller/`.
4. Add a row to "Reads from the API server" in
   `docs/src/content/docs/concepts/architecture.mdx`, and a sentence on the
   controller page.
