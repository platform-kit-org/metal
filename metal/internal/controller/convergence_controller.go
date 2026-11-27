package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/redact"
)

// proposalTTL marks a convergence proposal as one the operator may delete
// once it succeeded. The plan memory on the proposing object, not the
// operation record, carries the no-respawn guard, so the record may leave,
// but not before the site's retention (retention.go) runs out.
const proposalTTL = int32(24 * 60 * 60)

// ConvergenceReconciler drives desired state from the profile classes toward
// the machines, per host: it materializes the layered BiosProfile merge into
// a BiosSettings (and the discovered registry into a BiosSchema), observes
// drift, and PROPOSES a HostOperation — the human gate stays between every
// plan and its execution. Firmware converges before BIOS (the BIOS schema
// depends on firmware). An unchanged planHash never spawns a second
// operation: every hold below is alert-and-hold, unstuck by a profile edit.
//
// It reads the BMC only through a read-only session of its own, for the BIOS
// observation (attribute maps are too big to cache on the endpoint). It
// writes the Host's FirmwareConverged, SystemFirmwareConverged and
// BiosConverged conditions and both firmware plan memories — fields the
// HostReconciler leaves untouched.
type ConvergenceReconciler struct {
	client.Client
	// Audit writes the audit trail; nil writes none.
	Audit *audit.Logger
	// CredentialsNamespace is where credential and CA Secrets live.
	CredentialsNamespace string
	// OperationsNamespace is where proposals are created (the operator's own).
	OperationsNamespace string
	// Proposer is the operator's own username, stamped as proposedBy.
	Proposer string
	// PollInterval is the BIOS re-observation cadence.
	PollInterval time.Duration
	// Self keeps the operator from proposing a BIOS change, which reboots
	// the machine, on a machine it runs on (see SelfGuard); nil allows it.
	Self *SelfGuard
}

// +kubebuilder:rbac:groups=metal.platform-kit.org,resources=hosts,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.platform-kit.org,resources=hosts/status,verbs=patch
// +kubebuilder:rbac:groups=metal.platform-kit.org,resources=biosprofiles;firmwareprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.platform-kit.org,resources=managementendpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.platform-kit.org,resources=biossettings;biosschemas,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=metal.platform-kit.org,resources=biossettings/status,verbs=update
// +kubebuilder:rbac:groups=metal.platform-kit.org,resources=hostoperations,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=metal.platform-kit.org,resources=hostclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=os.platform-kit.org,resources=talosnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch,namespace=pk-metal

// Reconcile compares a Host's BIOS and firmware against the matching profiles
// and proposes operations for the difference. Proposing is create; the approval
// stays a person's patch, which this reconciler is not granted.
func (r *ConvergenceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	host := &metalv1alpha1.Host{}
	if err := r.Get(ctx, req.NamespacedName, host); err != nil {
		// Host gone — the owned BiosSettings/BiosSchema follow via GC.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// A host in maintenance belongs to a person: no reading its BIOS, no
	// proposals. The lanes pick up where they were when it comes back (the
	// annotation's removal is a Host event).
	if _, ok := inMaintenance(host); ok {
		return ctrl.Result{}, nil
	}

	biosProfiles, fwProfiles, err := r.matchedProfiles(ctx, host)
	if err != nil {
		return ctrl.Result{}, err
	}

	orig := host.DeepCopy()
	setCond := func(t string, s metav1.ConditionStatus, reason, msg string) {
		setCondition(&host.Status.Conditions, host.Generation, t, s, reason, msg)
	}

	// Manager firmware first, then the system's, then BIOS settings: each
	// can change what the next one offers.
	fwConverged, err := r.firmware(ctx, host, managerProfiles(fwProfiles), setCond)
	if err != nil {
		return ctrl.Result{}, err
	}
	sysConverged, err := r.systemFirmware(ctx, host, fwProfiles, fwConverged, setCond)
	if err != nil {
		return ctrl.Result{}, err
	}
	requeue, err := r.bios(ctx, host, biosProfiles, fwConverged && sysConverged, setCond)
	if err != nil {
		return ctrl.Result{}, err
	}

	// No phase here: the host reconciler is its one writer, and it re-derives
	// it after this write, which is a Host event. None of this lane's
	// conditions is an input of the phase.
	if !equality.Semantic.DeepEqual(orig.Status, host.Status) {
		if err := r.applyStatus(ctx, host); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	if requeue {
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// applyStatus declares only what this lane decides: its conditions and the
// firmware plan memory. The projected facts and the phase belong to the host
// reconciler and are left out entirely. A phase derived here would come from
// this pass's read of the host reconciler's conditions, which can be older
// than the phase that reconciler wrote last, and would replace it.
//
// Unlike the endpoint's identity lane, this one needs no uncached read. Losing
// the plan memory costs a re-proposal, and a re-proposal is harmless by
// construction: the operation is created before the memory is recorded and its
// name is derived from the plan hash, so the second attempt is an AlreadyExists
// and nothing is done twice.
func (r *ConvergenceReconciler) applyStatus(ctx context.Context, host *metalv1alpha1.Host) error {
	return applyOwnedStatus(ctx, r.Client, hostConvergenceShare(host), fieldOwnerHostConvergence)
}

// matchedProfiles selects both profile kinds against the host's labels.
func (r *ConvergenceReconciler) matchedProfiles(ctx context.Context, host *metalv1alpha1.Host) ([]metalv1alpha1.BiosProfile, []metalv1alpha1.FirmwareProfile, error) {
	bios := &metalv1alpha1.BiosProfileList{}
	if err := r.List(ctx, bios); err != nil {
		return nil, nil, err
	}
	fw := &metalv1alpha1.FirmwareProfileList{}
	if err := r.List(ctx, fw); err != nil {
		return nil, nil, err
	}
	matchedBios, err := selected(bios.Items, host.Labels, func(p *metalv1alpha1.BiosProfile) (string, metav1.LabelSelector) {
		return p.Name, p.Spec.Match
	})
	if err != nil {
		return nil, nil, err
	}
	matchedFw, err := selected(fw.Items, host.Labels, func(p *metalv1alpha1.FirmwareProfile) (string, metav1.LabelSelector) {
		return p.Name, p.Spec.Match
	})
	if err != nil {
		return nil, nil, err
	}
	return matchedBios, matchedFw, nil
}

// firmware converges the manager firmware and reports whether the lanes
// after it, system firmware and BIOS, may proceed (converged, or nothing
// desired). The observation is a pure cluster-state join — the endpoint poll
// already serves the manager version.
func (r *ConvergenceReconciler) firmware(ctx context.Context, host *metalv1alpha1.Host, profiles []metalv1alpha1.FirmwareProfile, setCond func(string, metav1.ConditionStatus, string, string)) (bool, error) {
	if len(profiles) == 0 {
		if err := r.withdrawPlan(ctx, &host.Status.FirmwareProposed, whyMovedOn); err != nil {
			return false, err
		}
		meta.RemoveStatusCondition(&host.Status.Conditions, metalv1alpha1.ConditionFirmwareConverged)
		host.Status.FirmwareProposed = nil
		return true, nil
	}
	target, conflict := mergeFirmware(profiles)
	if conflict != "" {
		setCond(metalv1alpha1.ConditionFirmwareConverged, metav1.ConditionFalse,
			metalv1alpha1.ReasonProfileConflict, conflict)
		return false, nil
	}

	versions := map[string]string{}
	push := false
	for _, p := range host.Status.ManagedBy {
		ep := &metalv1alpha1.ManagementEndpoint{}
		if err := r.Get(ctx, client.ObjectKey{Name: p.EndpointRef.Name}, ep); err != nil {
			continue
		}
		versions[ep.Name] = ep.Status.FirmwareVersion
		if caps := ep.Status.Capabilities; caps != nil && caps.UpdateDialect == string(bmc.DialectMultipartPush) {
			push = true
		}
	}
	if len(versions) == 0 {
		setCond(metalv1alpha1.ConditionFirmwareConverged, metav1.ConditionUnknown,
			metalv1alpha1.ReasonNoReachablePath, "no endpoint observation to compare against")
		return false, nil
	}
	drifted := ""
	for name, v := range versions {
		if v != target.Version {
			drifted = fmt.Sprintf("endpoint %s serves %s, want %s", name, v, target.Version)
			break
		}
	}
	if drifted == "" {
		setCond(metalv1alpha1.ConditionFirmwareConverged, metav1.ConditionTrue,
			metalv1alpha1.ReasonInSync, "manager "+target.Version)
		// The plan is spent: forgetting it is what lets a later drift to the
		// same desired state be proposed again instead of read as this
		// plan's failure.
		host.Status.FirmwareProposed = nil
		return true, nil
	}

	reason, msg, err := r.proposePlan(ctx, host, lanePlan{
		memory: &host.Status.FirmwareProposed, domain: "firmware",
		reason: metalv1alpha1.OperationReasonFirmwareConvergence, desired: target,
		stage: []metalv1alpha1.HostOperationAction{
			{FlashFirmware: &metalv1alpha1.FlashFirmwareAction{Image: target.Image, Version: target.Version}},
		},
		drift: drifted, noTransport: !push,
		requesters: func() ([]string, error) { return convergenceRequesters(ctx, r.Client, host, profiles) },
	})
	if err != nil {
		return false, err
	}
	setCond(metalv1alpha1.ConditionFirmwareConverged, metav1.ConditionFalse, reason, msg)
	return false, nil
}

// bios materializes the desired BIOS state, observes the machine, and
// converges — mirroring the settings' verdict onto the Host. Returns whether
// a re-observation requeue is wanted.
func (r *ConvergenceReconciler) bios(ctx context.Context, host *metalv1alpha1.Host, profiles []metalv1alpha1.BiosProfile, fwConverged bool, setCond func(string, metav1.ConditionStatus, string, string)) (bool, error) {
	if len(profiles) == 0 {
		settings := &metalv1alpha1.BiosSettings{}
		switch err := r.Get(ctx, client.ObjectKey{Name: host.Name}, settings); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return false, err
		default:
			if err := r.withdrawPlan(ctx, &settings.Status.Proposed, whyMovedOn); err != nil {
				return false, err
			}
			if err := r.Delete(ctx, settings); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		meta.RemoveStatusCondition(&host.Status.Conditions, metalv1alpha1.ConditionBiosConverged)
		return false, nil
	}

	attrs, provenance, conflicts := mergeBios(profiles)
	settings := &metalv1alpha1.BiosSettings{ObjectMeta: metav1.ObjectMeta{Name: host.Name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, settings, func() error {
		if err := controllerutil.SetOwnerReference(host, settings, r.Scheme()); err != nil {
			return err
		}
		// A conflicted merge holds the previous rendering whole — desired state
		// under dispute is never partially applied.
		if len(conflicts) == 0 {
			settings.Spec.Attributes = attrs
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("materialize biossettings %s: %w", host.Name, err)
	}

	origSettings := settings.DeepCopy()
	status, reason, msg := metav1.ConditionFalse, "", ""
	switch {
	case len(conflicts) > 0:
		reason, msg = metalv1alpha1.ReasonProfileConflict, strings.Join(conflicts, "; ")
	default:
		settings.Status.Provenance = provenance
		state, schema, obsErr := r.observeBios(ctx, host)
		if obsErr != nil {
			status = metav1.ConditionUnknown
			reason, msg = holdOf(obsErr, metalv1alpha1.ReasonBiosReadFailed)
			msg = redact.String(msg)
			break
		}
		if err := r.materializeSchema(ctx, host, state.Registry, schema); err != nil {
			return false, err
		}
		if violations := validateBios(settings.Spec.Attributes, schema); len(violations) > 0 {
			reason, msg = metalv1alpha1.ReasonSchemaViolation, strings.Join(violations, "; ")
			break
		}

		desired := decodeAttrs(settings.Spec.Attributes)
		settings.Status.Observed = subsetJSON(state.Attributes, desired)
		settings.Status.Pending = subsetJSON(state.Pending, desired)
		drifted := driftedKeys(desired, state.Attributes, state.Pending)
		if len(drifted) == 0 {
			status, reason = metav1.ConditionTrue, metalv1alpha1.ReasonInSync
			msg = fmt.Sprintf("%d attributes", len(desired))
			// The plan is spent — see the firmware lane.
			settings.Status.Proposed = nil
			break
		}
		if !fwConverged {
			reason = metalv1alpha1.ReasonAwaitingFirmware
			msg = "drift on " + strings.Join(drifted, ", ") + " — firmware converges first"
			break
		}
		var err error
		if reason, msg, err = r.proposePlan(ctx, host, lanePlan{
			memory: &settings.Status.Proposed, domain: "bios",
			reason: metalv1alpha1.OperationReasonBiosConvergence, desired: desired,
			// The attributes travel inside the operation: the approver sees
			// exactly what will be staged, and a later edit of the settings
			// cannot change an approved operation.
			stage: []metalv1alpha1.HostOperationAction{{ApplyBios: &metalv1alpha1.ApplyBiosAction{
				SettingsRef: &metalv1alpha1.Reference{Name: settings.Name},
				Attributes:  settings.Spec.Attributes,
			}}},
			reboots: true, drift: "drift on " + strings.Join(drifted, ", "),
			requesters: func() ([]string, error) { return convergenceRequesters(ctx, r.Client, host, profiles) },
		}); err != nil {
			return false, err
		}
	}

	setCondition(&settings.Status.Conditions, settings.Generation, metalv1alpha1.ConditionConverged, status, reason, msg)
	if err := updateStatusIfChanged(ctx, r.Client, settings, &origSettings.Status, &settings.Status); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	setCond(metalv1alpha1.ConditionBiosConverged, status, reason, msg)
	return true, nil
}

// lanePlan is a lane's drifted desired state, for proposePlan.
type lanePlan struct {
	memory      **metalv1alpha1.ProposedPlan // the lane's plan memory
	domain      string                       // the middle of the operation name
	reason      string                       // the operation's spec.reason
	desired     any                          // what the plan hash binds
	stage       []metalv1alpha1.HostOperationAction
	reboots     bool   // the change applies at a reset of the machine
	drift       string // what drifts, in words
	noTransport bool   // no path advertises the multipart-push transport
	requesters  func() ([]string, error)
}

// proposePlan proposes a lane's plan once, and returns the lane's condition
// reason and message. The steps run in one order for every lane:
//
//  1. A change that reboots a machine that runs a node drains the node first
//     (disruptivePlan). A node that is gone holds the plan, and the proposal
//     made for it is withdrawn.
//  2. A remembered plan with the same hash is not proposed again: its
//     operation, or the lack of one, gives the verdict (planVerdict).
//  3. A remembered plan with another hash is withdrawn while nobody approved
//     it, before any hold below.
//  4. The holds: no update transport; for a reboot, a disk that can still
//     hold a released node's install (powering it on would boot that node
//     back into its cluster), and the machine the operator runs on.
//  5. The operation is created, and then remembered.
func (r *ConvergenceReconciler) proposePlan(ctx context.Context, host *metalv1alpha1.Host, p lanePlan) (reason, msg string, err error) {
	var node *osv1alpha1.TalosNode
	if p.reboots {
		var nodeHold string
		if node, nodeHold, err = r.installedNode(ctx, host); err != nil {
			return "", "", err
		}
		if nodeHold != "" {
			// A proposal made while the node was there names it in every
			// drain step: take it back while nobody approved it yet.
			return metalv1alpha1.ReasonInstalledNodeMissing, p.drift + "; " + nodeHold, r.withdrawPlan(ctx, p.memory, whyNodeGone)
		}
	}
	hash := disruptiveHash(p.desired, node)
	if plan := *p.memory; plan != nil && plan.PlanHash == hash {
		var again bool
		if reason, msg, again, err = r.planVerdict(ctx, plan); err != nil || !again {
			return reason, msg, err
		}
		*p.memory = nil
	}
	if err = r.withdrawPlan(ctx, p.memory, whyMovedOn); err != nil {
		return "", "", err
	}
	if p.noTransport {
		return metalv1alpha1.ReasonNoUpdateTransport, p.drift + " — no path advertises the proven multipart-push transport", nil
	}
	actions := p.stage
	if p.reboots {
		if hold := notWipedPowerHold(host); hold != "" {
			return metalv1alpha1.ReasonDiskNotWiped, p.drift + "; " + hold, nil
		}
		var guard string
		if guard, err = r.Self.Check(ctx, host); err != nil {
			return "", "", err
		}
		if guard != "" {
			return metalv1alpha1.ReasonSelfDisruption, p.drift + "; no change that reboots the machine is proposed: " + guard, nil
		}
		actions = disruptivePlan(node, p.stage)
	}
	op := r.proposal(host.Name, p.domain, hash, p.reason, actions)
	if op.Spec.RequestedBy, err = p.requesters(); err != nil {
		return "", "", err
	}
	// Create before recording: a crash between the two re-proposes into
	// AlreadyExists (benign), the reverse order would hold on a ghost.
	if err = r.propose(ctx, op); err != nil {
		return "", "", err
	}
	ref := op.Namespace + "/" + op.Name
	logf.FromContext(ctx).Info("proposed operation", "operation", ref, "host", host.Name, "reason", p.reason)
	*p.memory = &metalv1alpha1.ProposedPlan{PlanHash: hash, Operation: ref}
	return metalv1alpha1.ReasonAwaitingOperation, p.drift + "; operation " + ref + " proposed", nil
}

// planVerdict derives a held plan's condition live from the operation or its
// absence — the outcome is never remembered, only that the proposal was made.
//
// again reports that a person asked for the plan once more: the failed
// operation carries the retry annotation (pk-metal retry). planVerdict then
// deletes that operation, and the caller forgets the plan so that this pass
// proposes it anew, as a new operation that waits for approval like the
// first; nothing runs without one. An operation that is simply gone was
// deleted by a person, which refuses it, and the plan stays spent until the
// profile changes. (A failed operation is not expired by its TTL, so a retry
// stays possible for as long as nobody deletes it.)
func (r *ConvergenceReconciler) planVerdict(ctx context.Context, plan *metalv1alpha1.ProposedPlan) (reason, msg string, again bool, err error) {
	ns, name, _ := strings.Cut(plan.Operation, "/")
	op := &metalv1alpha1.HostOperation{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, op); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", "", false, err
		}
		return metalv1alpha1.ReasonOperationGone,
			"proposal " + plan.Operation + " no longer exists: deleting a proposal refuses it — change the profile to re-propose", false, nil
	}
	c := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
	switch {
	case c == nil || c.Status == metav1.ConditionUnknown:
		return metalv1alpha1.ReasonAwaitingOperation, "operation " + plan.Operation + " in flight", false, nil
	case c.Status == metav1.ConditionFalse:
		if _, ok := op.Annotations[metalv1alpha1.AnnotationRetry]; ok {
			logf.FromContext(ctx).Info("retry requested; replacing the failed proposal", "operation", plan.Operation)
			// Preconditioned on the object read: a retry mark is honoured
			// for the failed operation it was set on, never for another
			// that took its name since.
			if err := deleteOp(ctx, r.Client, r.Audit, op, audit.DeleteRetry,
				"pk-metal retry marked it at "+op.Annotations[metalv1alpha1.AnnotationRetry]+"; a new proposal replaces it",
				client.Preconditions{UID: &op.UID}); client.IgnoreNotFound(err) != nil {
				return "", "", false, err
			}
			return "", "", true, nil
		}
		return metalv1alpha1.ReasonOperationFailed,
			"operation " + plan.Operation + " failed: " + c.Message + "; to propose it again: pk-metal retry " + plan.Operation, false, nil
	}
	return metalv1alpha1.ReasonConvergenceFailed,
		"operation " + plan.Operation + " succeeded but the observed state did not converge", false, nil
}

// propose creates the proposal, or adopts it when it already exists. The
// deterministic name dedupes a double-propose race; a same-named operation that
// is already finished belongs to an earlier round of the same plan (drift came
// back after the plan converged), so the proposal moves to the next round's
// name instead of mistaking that record for itself.
func (r *ConvergenceReconciler) propose(ctx context.Context, op *metalv1alpha1.HostOperation) error {
	base := op.Name
	for round := 1; round <= 64; round++ {
		if round > 1 {
			op.Name = fmt.Sprintf("%s-%d", base, round)
		}
		err := r.Create(ctx, op)
		if err == nil || !apierrors.IsAlreadyExists(err) {
			return err
		}
		existing := &metalv1alpha1.HostOperation{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(op), existing); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !existing.Finished() {
			return nil // this round's proposal — a double-propose race
		}
	}
	return fmt.Errorf("proposal %s: too many finished rounds of the same plan", base)
}

// proposal builds a convergence HostOperation. The deterministic name (host,
// domain, plan-hash prefix) makes proposing create-if-absent — the apiserver
// dedupes a double-propose race.
func (r *ConvergenceReconciler) proposal(hostName, domain, hash, reason string, actions []metalv1alpha1.HostOperationAction) *metalv1alpha1.HostOperation {
	return &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%s", hostName, domain, hash8(hash)),
			Namespace: r.OperationsNamespace,
		},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:                 metalv1alpha1.Reference{Name: hostName},
			ProposedBy:              r.Proposer,
			Reason:                  reason,
			PlanHash:                hash,
			Actions:                 actions,
			Approval:                metalv1alpha1.OperationApproval{Required: ptr.To(true)},
			TTLSecondsAfterFinished: ptr.To(proposalTTL),
		},
	}
}

// convergenceRequesters names the people who asked for what a convergence
// proposal applies (metal.platform-kit.org/requested-by): who last changed
// each profile it serves, and who last changed the host's claim when a
// profile selects the host by a label of the claim. The claim's labels
// are its intent: a change of them chose the profile for this machine.
func convergenceRequesters[P metalv1alpha1.BiosProfile | metalv1alpha1.FirmwareProfile](ctx context.Context, c client.Reader,
	host *metalv1alpha1.Host, profiles []P) ([]string, error) {
	annotations := make([]map[string]string, 0, len(profiles)+1)
	var selectors []metav1.LabelSelector
	for i := range profiles {
		switch p := any(&profiles[i]).(type) {
		case *metalv1alpha1.BiosProfile:
			annotations, selectors = append(annotations, p.Annotations), append(selectors, p.Spec.Match)
		case *metalv1alpha1.FirmwareProfile:
			annotations, selectors = append(annotations, p.Annotations), append(selectors, p.Spec.Match)
		}
	}
	if ref := host.Spec.ClaimRef; ref != nil {
		claim := &metalv1alpha1.HostClaim{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, claim); client.IgnoreNotFound(err) != nil {
			return nil, err
		} else if err == nil && selectsByClaim(selectors, claim) {
			annotations = append(annotations, claim.Annotations)
		}
	}
	return metalv1alpha1.RequestedBy(annotations...), nil
}

// selectsByClaim: one of the selectors names a label key that the claim
// puts on its host.
func selectsByClaim(selectors []metav1.LabelSelector, claim *metalv1alpha1.HostClaim) bool {
	for _, s := range selectors {
		for k := range s.MatchLabels {
			if _, ok := claim.Spec.Labels[k]; ok {
				return true
			}
		}
		for _, e := range s.MatchExpressions {
			if _, ok := claim.Spec.Labels[e.Key]; ok {
				return true
			}
		}
	}
	return false
}

// observeBios reads the machine's BIOS state and registry over a read-only
// session on the first reachable path.
func (r *ConvergenceReconciler) observeBios(ctx context.Context, host *metalv1alpha1.Host) (bmc.BiosState, []bmc.BiosAttribute, error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	cl, systemID, err := hostPathClient(cctx, r.Client, r.CredentialsNamespace, host, true)
	if err != nil {
		return bmc.BiosState{}, nil, err
	}
	defer cl.Close() //nolint:errcheck // read-only session
	state, err := cl.Bios(cctx, systemID)
	if err != nil {
		return bmc.BiosState{}, nil, err
	}
	schema, err := cl.BiosSchema(cctx, systemID)
	if err != nil {
		return bmc.BiosState{}, nil, err
	}
	return state, schema, nil
}

// materializeSchema renders the discovered registry into the host's
// BiosSchema. Serving no registry materializes nothing — absence is data, and
// an earlier discovery is not erased by it.
func (r *ConvergenceReconciler) materializeSchema(ctx context.Context, host *metalv1alpha1.Host, registry string, schema []bmc.BiosAttribute) error {
	if schema == nil {
		return nil
	}
	attrs := make([]metalv1alpha1.BiosSchemaAttribute, 0, len(schema))
	for _, a := range schema {
		attrs = append(attrs, metalv1alpha1.BiosSchemaAttribute{
			Name: a.Name, Type: a.Type, AllowedValues: a.AllowedValues,
		})
	}
	slices.SortFunc(attrs, func(a, b metalv1alpha1.BiosSchemaAttribute) int {
		return strings.Compare(a.Name, b.Name)
	})
	obj := &metalv1alpha1.BiosSchema{ObjectMeta: metav1.ObjectMeta{Name: host.Name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if err := controllerutil.SetOwnerReference(host, obj, r.Scheme()); err != nil {
			return err
		}
		obj.Spec.Registry = registry
		obj.Spec.Attributes = attrs
		return nil
	})
	if err != nil {
		return fmt.Errorf("materialize biosschema %s: %w", host.Name, err)
	}
	return nil
}

// driftedKeys lists the managed keys off their desired value, sorted. Three
// ways to drift: the observed value differs; the machine does not serve the
// key at all (the registry validated it, so staging is the honest attempt —
// the vendor verdict lands on the operation); or a CONTRADICTORY value sits
// staged in the pending object — current may match, but the next reboot will
// flip it, so it is future drift on a schedule and must be converged now.
func driftedKeys(desired, observed, pending map[string]any) []string {
	var drifted []string
	for k, want := range desired {
		got, ok := observed[k]
		staged, stagedOK := pending[k]
		if !ok || !jsonEqual(got, want) || (stagedOK && !jsonEqual(staged, want)) {
			drifted = append(drifted, k)
		}
	}
	slices.Sort(drifted)
	return drifted
}

func (r *ConvergenceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Each pass reads the BIOS over the BMC. The filters drop the status
	// writes that change no input of a lane: this reconciler's own
	// conditions, a step of the executor, a check-in of a node.
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.BiosSettings{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&metalv1alpha1.Host{}, handler.EnqueueRequestsFromMapFunc(sameName),
			builder.WithPredicates(convergenceHostEvents)).
		Watches(&metalv1alpha1.BiosProfile{}, handler.EnqueueRequestsFromMapFunc(r.allHosts)).
		Watches(&metalv1alpha1.FirmwareProfile{}, handler.EnqueueRequestsFromMapFunc(r.allHosts)).
		Watches(&metalv1alpha1.ManagementEndpoint{}, handler.EnqueueRequestsFromMapFunc(r.hostsBehindEndpoint)).
		Watches(&metalv1alpha1.HostOperation{}, handler.EnqueueRequestsFromMapFunc(hostForOperation),
			builder.WithPredicates(convergenceOperationEvents)).
		Watches(&metalv1alpha1.HostClaim{}, handler.EnqueueRequestsFromMapFunc(hostOfClaim)).
		// Only a node's coming, going and spec matter to the plan.
		Watches(&osv1alpha1.TalosNode{}, handler.EnqueueRequestsFromMapFunc(r.hostsRunningNode),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("convergence").
		Complete(r)
}

// convergenceHostEvents passes the Host events that change an input of a
// lane: the spec (claim, OS), the labels (profile selection), the
// annotations (maintenance, the guard allowances) and the management paths
// that hostPathClient dials.
var convergenceHostEvents = predicate.Or(
	predicate.GenerationChangedPredicate{},
	predicate.LabelChangedPredicate{},
	predicate.AnnotationChangedPredicate{},
	predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return !equality.Semantic.DeepEqual(e.ObjectOld.(*metalv1alpha1.Host).Status.ManagedBy,
			e.ObjectNew.(*metalv1alpha1.Host).Status.ManagedBy)
	}},
)

// convergenceOperationEvents passes the HostOperation events that change
// the verdict of a plan (planVerdict, withdrawPlan): a new or deleted
// operation, an approval (spec), a retry mark (annotation), and a new status
// of the Approved or Executed condition.
var convergenceOperationEvents = predicate.Or(
	predicate.GenerationChangedPredicate{},
	predicate.AnnotationChangedPredicate{},
	predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		old := e.ObjectOld.(*metalv1alpha1.HostOperation).Status.Conditions
		cur := e.ObjectNew.(*metalv1alpha1.HostOperation).Status.Conditions
		for _, t := range []string{metalv1alpha1.ConditionApproved, metalv1alpha1.ConditionExecuted} {
			if conditionStatus(old, t) != conditionStatus(cur, t) {
				return true
			}
		}
		return false
	}},
)

// conditionStatus is the status of the condition of type t, or "" when
// there is none.
func conditionStatus(conds []metav1.Condition, t string) metav1.ConditionStatus {
	if c := meta.FindStatusCondition(conds, t); c != nil {
		return c.Status
	}
	return ""
}

// sameName: the reconcile key IS the host name (BiosSettings share it).
func sameName(_ context.Context, obj client.Object) []ctrl.Request {
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: obj.GetName()}}}
}

// allHosts fans a profile event out to every host — matching is per-host.
func (r *ConvergenceReconciler) allHosts(ctx context.Context, _ client.Object) []ctrl.Request {
	hosts := &metalv1alpha1.HostList{}
	if err := r.List(ctx, hosts); err != nil {
		return nil
	}
	reqs := make([]ctrl.Request, 0, len(hosts.Items))
	for i := range hosts.Items {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: hosts.Items[i].Name}})
	}
	return reqs
}

// hostsBehindEndpoint re-converges when an endpoint's observations move — the
// firmware version after a flash arrives through the endpoint poll.
func (r *ConvergenceReconciler) hostsBehindEndpoint(_ context.Context, obj client.Object) []ctrl.Request {
	names := hostNames(obj.(*metalv1alpha1.ManagementEndpoint))
	reqs := make([]ctrl.Request, 0, len(names))
	for _, n := range names {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: n}})
	}
	return reqs
}
