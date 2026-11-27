------------------------------ MODULE ClaimHost ------------------------------
(***************************************************************************)
(* The hand-off of a Host between HostClaims: bind, release, unbind, and   *)
(* the Host phase that the Host controller writes while it happens.        *)
(*                                                                         *)
(* The model has one Host, the claims in Claims (each one created up to    *)
(* MaxGen times under the same name, with a new UID each time), the        *)
(* machine, one TalosNode per claim name, and the release operations.      *)
(*                                                                         *)
(* Actors:                                                                 *)
(*   - a person creates and deletes claims;                                *)
(*   - the claim controller (HostClaimReconciler) adds its finalizer,      *)
(*     binds, releases and unbinds;                                        *)
(*   - the Host controller (HostReconciler) projects OSReady from the      *)
(*     TalosNode and writes the Host phase, the only writer of it;         *)
(*   - the machine boots, its node checks in, a release operation powers   *)
(*     it off (approval and the executor's steps are abstracted away;      *)
(*     the Go model in protocol_model_test.go covers them);                *)
(*   - the informers deliver the events of each kind, in order, late.      *)
(*                                                                         *)
(* Caches. controller-runtime gives each kind one informer, and every      *)
(* controller of the manager reads it. A cache is behind the API server,   *)
(* never ahead of it, and never goes back. Two kinds are behind by         *)
(* different amounts. The model keeps, per kind, the snapshot the cache    *)
(* shows and the queue of snapshots not yet delivered.                     *)
(*                                                                         *)
(* Writes:                                                                 *)
(*   - Update (spec, metadata, claim status) carries the resourceVersion   *)
(*     of the object read. It fails with a conflict if the object changed  *)
(*     since, and the reconcile pass ends there. The model compares the    *)
(*     whole record, which also accepts a write after A-B-A changes: more  *)
(*     behaviours than the API server allows, so a safety proof here       *)
(*     holds there.                                                        *)
(*   - Server-side apply of the Host status (applyOwnedStatus) carries no  *)
(*     resourceVersion: a write computed from a stale read lands.          *)
(*                                                                         *)
(* A reconcile pass is one atomic step. Its reads come from the caches,    *)
(* except where the code reads through the API reader.                     *)
(*                                                                         *)
(* Rule selects the derivation of the Host phase facts:                    *)
(*   "pointer" - main: a claimRef to a claim that the cache does not hold  *)
(*               under that UID, or holds in deletion, is Releasing; the   *)
(*               OS ladder (Failed, Provisioning, Provisioned) needs a     *)
(*               claimRef.                                                 *)
(*   "cache"   - the derivation before the fix of the release race:        *)
(*               Releasing only while the cached claim exists and is in    *)
(*               deletion, and the OS ladder without a claimRef.           *)
(*               MC_cache.cfg keeps it, to prove that                      *)
(*               ReleasedHostNeverProvisioned catches the race.            *)
(*                                                                         *)
(* Not modelled: holds, maintenance, the self guard, Booted, a second      *)
(* Host, the executor's steps and approvals.                               *)
(*                                                                         *)
(* Each "\* go:" line names a Go function the operator below it mirrors,  *)
(* with a fingerprint of its code; TestSpecSync (spec/sync_test.go) fails  *)
(* when one of them changes. See README.md in the parent directory.        *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets

\* go-invariants: internal/envtest/staleread_claimhost_test.go

CONSTANTS
    Claims,  \* claim names
    MaxGen,  \* how often each name can be created
    Rule,    \* "cache" or "pointer", see above
    Nil

ASSUME Rule \in {"cache", "pointer"}
ASSUME MaxGen \in Nat \ {0}

VARIABLES
    claim,    \* API: Claims -> claim record
    host,     \* API: the Host
    node,     \* API: Claims -> Ready condition of the claim's TalosNode
    op,       \* API: release operations, by claim and generation
    machine,  \* the physical machine
    cache,    \* per kind: the snapshot the informer cache shows
    pend,     \* per kind: snapshots written, not yet delivered
    phaseFor  \* ghost: the claimRef the written Host phase was derived from

api  == <<claim, host, node, op>>
vars == <<claim, host, node, op, machine, cache, pend, phaseFor>>

Kinds   == {"claims", "hosts", "nodes", "ops"}
Snap    == [claims |-> claim, hosts |-> host, nodes |-> node, ops |-> op]

Gens    == 1..MaxGen
Refs    == Claims \X Gens            \* <<name, generation>>: a claim UID
Ref     == {Nil} \cup Refs
Phases  == {"Available", "Assigned", "Provisioning", "Provisioned", "Failed", "Releasing"}
\* The phases that say a claim holds the Host.
BoundPhases == {"Assigned", "Provisioning", "Provisioned", "Failed"}

ClaimRec == [exists: BOOLEAN, gen: 0..MaxGen, deleting: BOOLEAN, fin: BOOLEAN, bound: BOOLEAN]
HostRec  == [ref: Ref, os: {Nil} \cup Claims, powerOff: BOOLEAN,
             phase: Phases, osReady: {"absent", "Unknown", "True", "False"}]

TypeOK ==
    /\ claim \in [Claims -> ClaimRec]
    /\ host \in HostRec
    /\ node \in [Claims -> {"absent", "True", "False"}]
    /\ op \in [Refs -> {"none", "pending", "done"}]
    /\ machine \in [on: BOOLEAN, bootedFor: Ref]
    /\ phaseFor \in Ref

\* Every action that writes the API server appends what it wrote to the
\* queue of that kind. An unchanged kind gets no event.
Publish == pend' = [k \in Kinds |-> IF Snap'[k] # Snap[k]
                                    THEN Append(pend[k], Snap'[k]) ELSE pend[k]]

Live(r) == r # Nil /\ claim[r[1]].exists /\ claim[r[1]].gen = r[2]

\* A claim after its finalizer went: the object is gone, its last
\* generation stays so that the next create gets a new UID.
Gone(c) == [c EXCEPT !.exists = FALSE, !.deleting = FALSE, !.fin = FALSE, !.bound = FALSE]

\* releaseAll: the claimRef, the OS hand-off and the power-off record go.
Unbound(h) == [h EXCEPT !.ref = Nil, !.os = Nil, !.powerOff = FALSE]

-----------------------------------------------------------------------------
(* The Host phase: phase.ForHost and factsFor, restricted to the inputs    *)
(* that the hand-off moves. Inspected and Verified are True; Maintenance   *)
(* and Booted are not modelled.                                            *)

\* go: phase.ForHost dc136979b851
ForHost(osReady, claimed, releasing) ==
    IF releasing THEN "Releasing"
    ELSE IF osReady = "False" /\ (Rule = "cache" \/ claimed) THEN "Failed"
    ELSE IF osReady = "True" /\ (Rule = "cache" \/ claimed) THEN "Provisioned"
    ELSE IF claimed /\ osReady # "absent" THEN "Provisioning"
    ELSE IF claimed THEN "Assigned"
    ELSE "Available"

\* The facts as one controller derives them from the cached Host h and the
\* cached claims cc: <<claimed, releasing>>.
\* go: controller.factsFor bad752278fb5
Facts(h, cc) ==
    LET held == h.ref # Nil /\ cc[h.ref[1]].exists /\ cc[h.ref[1]].gen = h.ref[2]
        claimed == h.ref # Nil
    IN  <<claimed,
          IF Rule = "cache" THEN held /\ cc[h.ref[1]].deleting
                            ELSE claimed /\ (~held \/ cc[h.ref[1]].deleting)>>

\* projectOS: OSReady mirrors the Ready condition of the hand-off's node.
\* go: controller.projectOS cea7f4b857c8
OSReadyOf(h, nodes) ==
    IF h.os = Nil THEN "absent"
    ELSE CASE nodes[h.os] = "absent" -> "Unknown"
           [] nodes[h.os] = "True"   -> "True"
           [] nodes[h.os] = "False"  -> "False"

-----------------------------------------------------------------------------
(* A person *)

CreateClaim(c) ==
    /\ ~claim[c].exists
    /\ claim[c].gen < MaxGen
    /\ claim' = [claim EXCEPT ![c] = [exists |-> TRUE, gen |-> @.gen + 1,
                                      deleting |-> FALSE, fin |-> FALSE, bound |-> FALSE]]
    /\ UNCHANGED <<host, node, op, machine, cache, phaseFor>>
    /\ Publish

\* A claim with the finalizer is marked; one without it goes at once.
DeleteClaim(c) ==
    /\ claim[c].exists
    /\ ~claim[c].deleting
    /\ claim' = [claim EXCEPT ![c] = IF @.fin THEN [@ EXCEPT !.deleting = TRUE] ELSE Gone(@)]
    /\ UNCHANGED <<host, node, op, machine, cache, phaseFor>>
    /\ Publish

-----------------------------------------------------------------------------
(* The claim controller: HostClaimReconciler.Reconcile *)

\* The cached claim exists and is not being deleted: AddFinalizer, bind,
\* writeStatus. Each write is an Update; the first conflict ends the pass.
\* go: controller.HostClaimReconciler.Reconcile 0e364a68365c
\* go: controller.HostClaimReconciler.bind ef2dbcf6d76d
\* go: controller.HostClaimReconciler.writeStatus eaf9514465d0
ClaimBind(c) ==
    LET cl   == cache.claims[c]
        h    == cache.hosts
        me   == <<c, cl.gen>>
        mine == [cl EXCEPT !.fin = TRUE]
        \* bind: free when no claimRef, or a claimRef of a deleted claim of
        \* the same name (another UID) whose pointer is taken over.
        ours == h.ref = me
        free == h.ref = Nil \/ (h.ref[1] = c /\ h.ref[2] # cl.gen)
        bindOK   == free /\ host = h
        conflict == free /\ host # h
        bound    == ours \/ bindOK
    IN  /\ cl.exists /\ ~cl.deleting
        \* AddFinalizer: an Update of the cached claim. A conflict here
        \* ends the pass with nothing written.
        /\ claim[c] = cl
        /\ host' = IF bindOK THEN [host EXCEPT !.ref = me, !.os = c] ELSE host
        /\ claim' = [claim EXCEPT ![c] = IF conflict THEN mine ELSE [mine EXCEPT !.bound = bound]]
        /\ UNCHANGED <<node, op, machine, cache, phaseFor>>
        /\ Publish
        /\ api' # api

\* The cached claim is being deleted and carries the finalizer:
\* finishRelease and release. The hosts come from the API reader
\* (liveBoundHosts); the release operations from the cache.
\* go: controller.HostClaimReconciler.finishRelease 58b8a9b818a1
\* go: controller.HostClaimReconciler.liveBoundHosts d4c505783ee8
\* go: controller.HostClaimReconciler.release b7ba6a9d9c48
\* go: controller.HostClaimReconciler.recordPowerOff c0f1bdd325c8
\* go: controller.HostClaimReconciler.releaseAll b0137316430c
\* go: controller.runsNode 0029e5e5ef0d
\* go: controller.powerOffDecided 025b2a319b02
ClaimRelease(c) ==
    LET cl      == cache.claims[c]
        live    == host
        me      == <<c, cl.gen>>
        \* liveBoundHosts filters by name; release skips another UID.
        holds   == live.ref = me
        cop     == cache.ops[me]
        \* powerOff: an operation exists, runsNode, bootRan, powerOffDecided.
        \* bootRanUncached reads the boot from the API server; the boot
        \* records powerOff before its baseline, so the live Host has it.
        needOff == cop # "none" \/ live.osReady = "True" \/ live.powerOff
        \* RemoveFinalizer: an Update of the cached claim.
        release == IF claim[c] = cl THEN [claim EXCEPT ![c] = Gone(@)] ELSE claim
    IN  /\ cl.exists /\ cl.deleting /\ cl.fin
        /\ \/ /\ ~holds
              /\ claim' = release
              /\ UNCHANGED <<host, op>>
           \/ /\ holds /\ ~needOff
              /\ host' = Unbound(host)
              /\ claim' = release
              /\ UNCHANGED op
           \/ /\ holds /\ needOff /\ cop # "done"
              \* recordPowerOff, then the release operation; a create of one
              \* the cache does not show yet is AlreadyExists.
              /\ host' = [host EXCEPT !.powerOff = TRUE]
              /\ op' = IF cop = "none" /\ op[me] = "none" THEN [op EXCEPT ![me] = "pending"] ELSE op
              /\ UNCHANGED claim
           \/ /\ holds /\ needOff /\ cop = "done"
              /\ host' = Unbound(host)
              /\ claim' = release
              /\ UNCHANGED op
        /\ UNCHANGED <<node, machine, cache, phaseFor>>
        /\ Publish
        /\ api' # api

\* The cache holds no claim of that name: unbind what the cached index
\* still lists under the name, with an Update of the cached Host.
\* go: controller.HostClaimReconciler.releaseAll b0137316430c
ClaimGone(c) ==
    LET h == cache.hosts
    IN  /\ ~cache.claims[c].exists
        /\ h.ref # Nil /\ h.ref[1] = c
        /\ host = h
        /\ host' = Unbound(host)
        /\ UNCHANGED <<claim, node, op, machine, cache, phaseFor>>
        /\ Publish

-----------------------------------------------------------------------------
(* The writer of the Host phase *)

\* HostReconciler.Reconcile: projectOS from the cached node, factsFor with
\* the cached claim, StampHost, and a status apply when the result differs
\* from the cached status.
\* go: controller.HostReconciler.Reconcile c5f429c505a7
\* go: controller.HostReconciler.applyStatus 1f380381ef89
HostReconcile ==
    LET h  == cache.hosts
        f  == Facts(h, cache.claims)
        os == OSReadyOf(h, cache.nodes)
        ph == ForHost(os, f[1], f[2])
    IN  /\ <<ph, os>> # <<h.phase, h.osReady>>
        /\ host' = [host EXCEPT !.phase = ph, !.osReady = os]
        /\ phaseFor' = h.ref
        /\ UNCHANGED <<claim, node, op, machine, cache>>
        /\ Publish

-----------------------------------------------------------------------------
(* The machine and the executor *)

\* The boot operation starts (recordBootStart reads the Host and the claim
\* from the API server and records powerOff first) and boots the machine.
\* go: controller.HostOperationReconciler.recordBootStart 659c75b8f853
Boot ==
    LET r == host.ref
    IN  /\ Live(r)
        /\ ~claim[r[1]].deleting
        /\ host.os = r[1]
        /\ machine.bootedFor # r
        /\ host' = [host EXCEPT !.powerOff = TRUE]
        /\ machine' = [on |-> TRUE, bootedFor |-> r]
        /\ UNCHANGED <<claim, node, op, cache, phaseFor>>
        /\ Publish

\* The node of the booted claim checks in.
NodeUp(c) ==
    /\ machine.on /\ machine.bootedFor # Nil /\ machine.bootedFor[1] = c
    /\ node[c] # "True"
    /\ node' = [node EXCEPT ![c] = "True"]
    /\ UNCHANGED <<claim, host, op, machine, cache, phaseFor>>
    /\ Publish

\* A node whose machine is off, or runs another claim's node, reads not ready.
NodeDown(c) ==
    /\ node[c] = "True"
    /\ ~(machine.on /\ machine.bootedFor # Nil /\ machine.bootedFor[1] = c)
    /\ node' = [node EXCEPT ![c] = "False"]
    /\ UNCHANGED <<claim, host, op, machine, cache, phaseFor>>
    /\ Publish

\* A person approved the release; the executor powers the machine off.
PowerOff(r) ==
    /\ op[r] = "pending"
    /\ op' = [op EXCEPT ![r] = "done"]
    /\ machine' = [machine EXCEPT !.on = FALSE]
    /\ UNCHANGED <<claim, host, node, cache, phaseFor>>
    /\ Publish

-----------------------------------------------------------------------------
(* The informers *)

Deliver(k) ==
    /\ pend[k] # <<>>
    /\ cache' = [cache EXCEPT ![k] = Head(pend[k])]
    /\ pend' = [pend EXCEPT ![k] = Tail(@)]
    /\ UNCHANGED <<claim, host, node, op, machine, phaseFor>>

-----------------------------------------------------------------------------

Init ==
    /\ claim = [c \in Claims |-> [exists |-> FALSE, gen |-> 0, deleting |-> FALSE,
                                  fin |-> FALSE, bound |-> FALSE]]
    /\ host = [ref |-> Nil, os |-> Nil, powerOff |-> FALSE,
               phase |-> "Available", osReady |-> "absent"]
    /\ node = [c \in Claims |-> "absent"]
    /\ op = [r \in Refs |-> "none"]
    /\ machine = [on |-> FALSE, bootedFor |-> Nil]
    /\ cache = Snap
    /\ pend = [k \in Kinds |-> <<>>]
    /\ phaseFor = Nil

Next ==
    \/ \E c \in Claims :
          \/ CreateClaim(c) \/ DeleteClaim(c)
          \/ ClaimBind(c) \/ ClaimRelease(c) \/ ClaimGone(c)
          \/ NodeUp(c) \/ NodeDown(c)
    \/ HostReconcile
    \/ Boot
    \/ \E r \in Refs : PowerOff(r)
    \/ \E k \in Kinds : Deliver(k)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
(* Invariants *)

\* A claim that reports Bound and is not being deleted holds the Host. So at
\* most one live claim reports Bound: the Host has one claimRef.
BoundClaimHoldsHost ==
    \A c \in Claims :
        claim[c].exists /\ ~claim[c].deleting /\ claim[c].bound => host.ref = <<c, claim[c].gen>>

\* The finalizer goes only after the Host no longer carries the claim.
FinalizerLast ==
    \A c \in Claims :
        ~claim[c].exists /\ claim[c].gen > 0 => host.ref # <<c, claim[c].gen>>

(* Properties of steps *)

\* No claim takes over the Host from a claim that still exists.
NoSteal ==
    [][host.ref # Nil /\ host'.ref # Nil /\ host'.ref # host.ref => ~Live(host.ref)]_vars

\* A machine that a boot started is unbound only once it is off.
PowerOffBeforeUnbind ==
    [][host.ref # Nil /\ host'.ref # host.ref /\ machine.bootedFor = host.ref => ~machine.on]_vars

\* No controller writes a phase that says a claim holds the Host, derived
\* from a bind that the API server already ended. This is the release race
\* seen in the lab: Releasing, Provisioned, Available.
ReleasedHostNeverProvisioned ==
    [][host'.phase # host.phase /\ host'.phase \in BoundPhases => phaseFor' = host.ref]_vars

=============================================================================
