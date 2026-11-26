---------------------------------- MODULE MC ----------------------------------
(* The bounds of a TLC run, and the states each run must reach. ClaimHost.tla *)
(* holds the model; hack/tlc.sh runs the configurations.                      *)
EXTENDS ClaimHost, TLC

CONSTANT MaxLag \* how many writes of one kind a cache can be behind

\* The claims are interchangeable: a, b and b, a are one state.
Symmetry == Permutations(Claims)

\* A state constraint: TLC does not go past a state where a cache is more
\* than MaxLag writes behind.
LagBound == \A k \in Kinds : Len(pend[k]) <= MaxLag

(* Reachability. A model that cannot reach the states the invariants are   *)
(* about proves nothing. Each Never* below is false somewhere in the       *)
(* model: hack/tlc.sh runs each one as an invariant and requires TLC to    *)
(* report it violated.                                                     *)

\* The node of a claim checked in, and the Host says so.
NeverProvisioned == host.phase # "Provisioned"
\* A release powered the machine off.
NeverPoweredOff == \A r \in Refs : op[r] # "done"
\* A stale Host cache wrote Releasing after the API server unbound the Host:
\* the race of the lab, with the derivation of main.
NeverStaleReleasing == ~(host.ref = Nil /\ host.phase = "Releasing")
\* One claim was released, and the other claim holds the Host.
NeverRebound == ~(\E c \in Claims : claim[c].gen > 0 /\ ~claim[c].exists /\ host.ref # Nil /\ host.ref[1] # c)
\* A claim of the same name, created again, took over the pointer.
NeverTakeover == ~(\E c \in Claims : host.ref = <<c, 2>>)
===============================================================================
