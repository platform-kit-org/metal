// Package phase derives status.phase and status.blockedOn — pure functions of
// conditions + spec-derived facts, nothing else (conditions are truth, phase
// is a human-facing summary, written but never read). The Stamp helpers are the ONLY
// write sites: reconcilers call them, and TestPhaseWriteSites (mise run lint:phase)
// fails on any other write anywhere in the module and on any read in a reconciler.
//
// The derivation is a precedence list, not a state machine — there is no memory of a
// previous phase, every call re-derives from observations. Phases whose inputs are
// not observations (claimed, releasing, maintenance) hang off HostFacts booleans the
// owning reconcilers compute from spec and metadata.
//
// Each kind has one writer of its phase, the reconciler that observes it. A
// second writer would derive the phase from its own read of the first writer's
// conditions; that read can be older than the phase already written, and the
// phase would then step back.
package phase

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// ForEndpoint derives the ManagementEndpoint phase. Precedence: hard stops first
// (Unreachable, Mismatch), then the pipeline position in observation order
// (Inventorying → Verifying → Configuring → Ready). Each observing pass sets
// Reachable before the phase is stamped.
func ForEndpoint(conds []metav1.Condition) (metalv1alpha1.EndpointPhase, string) {
	blocked := blockedOn(conds)
	if !meta.IsStatusConditionTrue(conds, metalv1alpha1.ConditionReachable) {
		return metalv1alpha1.EndpointUnreachable, blocked
	}

	if v := meta.FindStatusCondition(conds, metalv1alpha1.ConditionVerified); v != nil && v.Status == metav1.ConditionFalse {
		return metalv1alpha1.EndpointMismatch, blocked
	}
	if !meta.IsStatusConditionTrue(conds, metalv1alpha1.ConditionInventoried) {
		return metalv1alpha1.EndpointInventorying, blocked
	}
	if !meta.IsStatusConditionTrue(conds, metalv1alpha1.ConditionVerified) {
		return metalv1alpha1.EndpointVerifying, blocked
	}
	// Configured is profile-driven: absent means no EndpointProfile matched — that is
	// Ready, not an eternal Configuring.
	if c := meta.FindStatusCondition(conds, metalv1alpha1.ConditionConfigured); c != nil && c.Status != metav1.ConditionTrue {
		if blocked == "" && c.Status == metav1.ConditionFalse && configFailures[c.Reason] {
			blocked = c.Reason
		}
		return metalv1alpha1.EndpointConfiguring, blocked
	}
	return metalv1alpha1.EndpointReady, blocked
}

// configFailures are the Configured=False reasons that the operator cannot
// clear alone. The other False reasons are work in flight.
var configFailures = map[string]bool{
	metalv1alpha1.ReasonRotationFailed:      true,
	metalv1alpha1.ReasonAccountRetireFailed: true,
	metalv1alpha1.ReasonNTPFailed:           true,
	metalv1alpha1.ReasonProfileConflict:     true,
	metalv1alpha1.ReasonIdentityPushFailed:  true,
	metalv1alpha1.ReasonNoIdentityTransport: true,
	metalv1alpha1.ReasonTLSNotVerified:      true,
}

// HostFacts are the spec-derived inputs to the Host derivation — booleans the owning
// reconcilers compute (Claimed from spec.claimRef, Releasing from the claim's
// deletion or absence, Maintenance from the metal.platform-kit.org/maintenance annotation). They keep the function total: every phase in the enum is reachable
// and tested.
type HostFacts struct {
	// Claimed: a HostClaim is bound to this host.
	Claimed bool
	// Releasing: the claim that spec.claimRef names is in deletion or gone, and
	// cleanup (wipe, eject, unbind) is underway.
	Releasing bool
	// Maintenance: a person parked the machine outside the lifecycle
	// (annotation metal.platform-kit.org/maintenance).
	Maintenance bool
}

// ForHost derives the Host phase. Precedence: parked states first (Maintenance,
// Releasing), then the ladder downwards (Provisioned → Provisioning → Assigned
// → Available → Inspecting). The OS rungs read OSReady, which the host
// reconciler sets while the Host has a hand-off: True when the node is ready,
// Unknown otherwise. Each pass sets Inspected before the phase is stamped.
func ForHost(conds []metav1.Condition, f HostFacts) (metalv1alpha1.HostPhase, string) {
	blocked := blockedOn(conds)

	switch {
	case f.Maintenance:
		// An alarm (a failed verification) still outranks the person's
		// own reason for the stop.
		if blocked == "" {
			blocked = metalv1alpha1.ReasonHostInMaintenance
		}
		return metalv1alpha1.HostMaintenance, blocked
	case f.Releasing:
		return metalv1alpha1.HostReleasing, blocked
	}

	// The OS ladder describes the claim's install, so a free host has none.
	// The host reconciler drops OSReady with the hand-off, but a read from
	// before that pass still has it.
	if f.Claimed {
		osReady := meta.FindStatusCondition(conds, metalv1alpha1.ConditionOSReady)
		switch {
		case osReady != nil && osReady.Status == metav1.ConditionTrue:
			return metalv1alpha1.HostProvisioned, blocked
		case osReady != nil:
			return metalv1alpha1.HostProvisioning, blocked
		}
		return metalv1alpha1.HostAssigned, blocked
	}

	if meta.IsStatusConditionTrue(conds, metalv1alpha1.ConditionInspected) &&
		meta.IsStatusConditionTrue(conds, metalv1alpha1.ConditionVerified) {
		return metalv1alpha1.HostAvailable, blocked
	}
	return metalv1alpha1.HostInspecting, blocked
}

// ForClaim derives the HostClaim phase from its Bound condition. Reference
// resolution maps 0/1/2+ matches to Pending/Bound/Ambiguous (naming rule 9);
// HostNotDiscovered is in flight, not blocked — the machine just isn't there yet.
// An install operation that waits for its gate shows as Approval(Provisioning),
// the shape the Host uses for the same wait.
func ForClaim(conds []metav1.Condition) (metalv1alpha1.ClaimPhase, string) {
	b := meta.FindStatusCondition(conds, metalv1alpha1.ConditionBound)
	switch {
	case b == nil:
		return metalv1alpha1.ClaimPending, ""
	case b.Status == metav1.ConditionTrue:
		// Bound; whatever stops provisioning is why the claim isn't advancing.
		p := meta.FindStatusCondition(conds, metalv1alpha1.ConditionProvisioned)
		switch {
		case p == nil || p.Status != metav1.ConditionFalse:
		case p.Reason == metalv1alpha1.ReasonAwaitingApproval:
			return metalv1alpha1.ClaimBound, "Approval(Provisioning)"
		case provisioningHolds[p.Reason]:
			return metalv1alpha1.ClaimBound, p.Reason
		}
		return metalv1alpha1.ClaimBound, ""
	case b.Reason == metalv1alpha1.ReasonAmbiguousSystem:
		return metalv1alpha1.ClaimAmbiguous, b.Reason
	case b.Reason == metalv1alpha1.ReasonHostNotDiscovered:
		return metalv1alpha1.ClaimPending, ""
	}
	return metalv1alpha1.ClaimPending, b.Reason
}

// provisioningHolds are the Provisioned=False reasons that show in
// blockedOn: a stop that a person resolves, or a wait for an etcd turn
// (EtcdJoinWaiting, RemovalWaiting). The rest are in-flight waits.
var provisioningHolds = map[string]bool{
	metalv1alpha1.ReasonSingleMediumUnsupported: true,
	metalv1alpha1.ReasonNoBootableMedia:         true,
	metalv1alpha1.ReasonOperationFailed:         true,
	metalv1alpha1.ReasonReleaseFailed:           true,
	metalv1alpha1.ReasonHeldByUser:              true,
	metalv1alpha1.ReasonHostInMaintenance:       true,
	metalv1alpha1.ReasonSelfDisruption:          true,
	metalv1alpha1.ReasonDiskNotWiped:            true,
	metalv1alpha1.ReasonEtcdRiskRefused:         true,
	metalv1alpha1.ReasonEtcdJoinWaiting:         true,
	metalv1alpha1.ReasonRemovalWaiting:          true,
}

// ForOperation derives the HostOperation phase from Approved + Executed. A
// finished Executed wins (Succeeded/Failed/Aborted are terminal); an unstamped gate is
// AwaitingApproval; a queued operation stays Proposed (blocked on its
// predecessor); an approved, unfinished operation is Running.
func ForOperation(conds []metav1.Condition) (metalv1alpha1.OperationPhase, string) {
	ex := meta.FindStatusCondition(conds, metalv1alpha1.ConditionExecuted)
	switch {
	case ex != nil && ex.Status == metav1.ConditionTrue:
		return metalv1alpha1.OperationSucceeded, ""
	case ex != nil && ex.Status == metav1.ConditionFalse && ex.Reason == metalv1alpha1.ReasonAborted:
		return metalv1alpha1.OperationAborted, ""
	case ex != nil && ex.Status == metav1.ConditionFalse:
		return metalv1alpha1.OperationFailed, ""
	}
	ap := meta.FindStatusCondition(conds, metalv1alpha1.ConditionApproved)
	switch {
	case ap == nil:
		return metalv1alpha1.OperationProposed, ""
	case ap.Status != metav1.ConditionTrue:
		return metalv1alpha1.OperationAwaitingApproval, ap.Reason
	}
	if ex != nil && ex.Reason != metalv1alpha1.ReasonExecuting {
		// Approved but held: queued behind a predecessor, or the host/paths are
		// not there — the reason says which.
		return metalv1alpha1.OperationProposed, ex.Reason
	}
	return metalv1alpha1.OperationRunning, ""
}

// blockedOn derives WHY the object isn't advancing — a reason, not a phase, because it
// recurs across phases. Precedence: a failed verification outranks a pending approval.
func blockedOn(conds []metav1.Condition) string {
	if v := meta.FindStatusCondition(conds, metalv1alpha1.ConditionVerified); v != nil && v.Status == metav1.ConditionFalse {
		return v.Reason
	}
	if ap := meta.FindStatusCondition(conds, metalv1alpha1.ConditionApprovalPending); ap != nil && ap.Status == metav1.ConditionTrue {
		// Reason names the gated domain (Bios, Firmware, Reboot…); rendered in the
		// documented Approval(bios) shape. Condition reasons cannot carry parens.
		return fmt.Sprintf("Approval(%s)", ap.Reason)
	}
	// A machine that may still hold a node's install waits for a person's
	// wipe before anything else is done with it.
	if dw := meta.FindStatusCondition(conds, metalv1alpha1.ConditionDiskWiped); dw != nil && dw.Status == metav1.ConditionFalse {
		return dw.Reason
	}
	return ""
}

// StampEndpoint derives and writes phase + blockedOn — the single endpoint write site.
func StampEndpoint(ep *metalv1alpha1.ManagementEndpoint) {
	ep.Status.Phase, ep.Status.BlockedOn = ForEndpoint(ep.Status.Conditions)
}

// StampHost derives and writes phase + blockedOn — the single host write site.
func StampHost(h *metalv1alpha1.Host, f HostFacts) {
	h.Status.Phase, h.Status.BlockedOn = ForHost(h.Status.Conditions, f)
}

// StampClaim derives and writes phase + blockedOn — the single claim write site.
func StampClaim(c *metalv1alpha1.HostClaim) {
	c.Status.Phase, c.Status.BlockedOn = ForClaim(c.Status.Conditions)
}

// StampOperation derives and writes phase + blockedOn — the single operation
// write site.
func StampOperation(op *metalv1alpha1.HostOperation) {
	op.Status.Phase, op.Status.BlockedOn = ForOperation(op.Status.Conditions)
}
