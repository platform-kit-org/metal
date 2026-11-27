package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/imager/quirks"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/boot"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// The provisioning lane: a bound claim whose OS intent names a rendered node is
// proposed ONE boot operation that mounts the installer and the node's config
// medium, points the next boot at them, powers the machine and holds the media
// until the node checks in. A BMC that holds one medium gets one installer
// with the node's config built in instead, on its one slot: the config still
// travels out of band, over the same granted media path. Deleting the claim releases the machine through an
// operator-created power-off. Both live on the claim reconciler: the claim is
// what both decisions hang off, and its status has a single writer.
const (
	// releaseFinalizer holds a deleted claim until its machine is handed back.
	releaseFinalizer = "metal.platform-kit.org/release"

	// Labels joining the operator's operations back to the claim they serve —
	// operations live in the operator's namespace, claims wherever they are.
	labelClaimNamespace = "metal.platform-kit.org/claim-namespace"
	labelClaimName      = "metal.platform-kit.org/claim-name"
	labelClaimUID       = "metal.platform-kit.org/claim-uid"
	labelPurpose        = "metal.platform-kit.org/purpose"
	purposeProvision    = "provision"
	purposeRelease      = "release"

	// annotationConfigDigest on a boot operation names the rendered config
	// it boots, so its success can be recorded with the digest.
	annotationConfigDigest = "metal.platform-kit.org/config-digest"

	// annotationReleasePowerOff on a Host names the UID of the claim whose
	// release powers the machine off. The executor writes it when a boot of
	// the claim starts, before its first BMC write; the release writes it
	// when it decides. It keeps that decision after the records it was read
	// from are gone: the boot operations are deleted to stop the boot or by
	// pk-metal retry, and the release operation itself can be deleted by
	// pk-metal retry or by its TTL. Without it, the next pass would find no
	// boot and hand back a machine that is still mid-boot. releaseAll
	// removes it when the host is unbound.
	annotationReleasePowerOff = "metal.platform-kit.org/release-power-off"

	// labelProvisioning marks BootAssets the lane created for itself.
	labelProvisioning = "metal.platform-kit.org/provisioning"

	// Operation reasons — the Approval(<reason>) domain on the Host.
	reasonProvisioning = "Provisioning"
	reasonRelease      = "Release"

	// releaseTTL marks a release operation as one the operator may delete
	// once it succeeded: nothing guards on its existence once the claim is
	// gone. It is the least time the record stays; the site's retention
	// (retention.go) keeps it longer.
	releaseTTL = int32(24 * 60 * 60)
)

// provisionPlan is everything one boot operation commits to. Its hash names the
// operation, so the same plan never spawns twice and a changed render, image or
// endpoint shape is a new plan.
type provisionPlan struct {
	ConfigDigest   string `json:"configDigest"`
	ArtifactDigest string `json:"artifactDigest"`
	Installer      string `json:"installer,omitempty"`
	// InstallerDigest pins the installer the operator downloaded from
	// Installer; the media service serves it under a grant.
	InstallerDigest string   `json:"installerDigest,omitempty"`
	Slots           []string `json:"slots"`
	Target          string   `json:"target,omitempty"`
	Mode            string   `json:"mode,omitempty"`
	// Embedded: the one medium is an installer with the config built in;
	// ArtifactDigest is its digest. Omitted when false, so the plans of the
	// other shapes keep their hashes.
	Embedded bool `json:"embedded,omitempty"`
	// Replaces names the install the boot replaces, <node>@<hand-off time>:
	// a reinstall of the same node, or another node the claim's machine ran
	// before. Its operation first takes that node out of its cluster.
	// Omitted for a first install, so those plans keep their hashes.
	Replaces string `json:"replaces,omitempty"`
}

// bootShape is the kind of boot an endpoint and a machine allow.
type bootShape int

const (
	// shapeTwoMedia: the installer on a boot slot, the config medium on
	// another slot.
	shapeTwoMedia bootShape = iota
	// shapeConfigOnly: one slot, and the machine already runs Talos from its
	// disk (it answers in maintenance mode): the config medium alone.
	shapeConfigOnly
	// shapeEmbedded: one slot and a blank machine: one installer with the
	// node's config built in.
	shapeEmbedded
)

// decideShape picks the boot for an endpoint with the given slots and a
// machine that does or does not answer. It returns a hold reason and message
// when no boot this operator can make fits.
func decideShape(hostName string, v endpointView, reachable bool, talosVersion string) (bootShape, string, string) {
	_, configSlot := v.mediaSlots()
	switch {
	case configSlot != "":
		return shapeTwoMedia, "", ""
	case reachable:
		return shapeConfigOnly, "", ""
	case len(v.bootSlots) == 0:
		return 0, metalv1alpha1.ReasonSingleMediumUnsupported, fmt.Sprintf(
			"host %s can hold one virtual medium only (slots %v), and none of them boots an ISO; a blank machine needs one that does",
			hostName, v.insertable)
	case !quirks.New(talosVersion).SupportsEmbeddedConfig():
		return 0, metalv1alpha1.ReasonSingleMediumUnsupported, fmt.Sprintf(
			"host %s can hold one virtual medium only (slots %v); a blank machine then needs an installer with its config built in, which Talos %s cannot read (Talos 1.12 or later can)",
			hostName, v.insertable, talosVersion)
	}
	return shapeEmbedded, "", ""
}

// provision drives the claim's Provisioned condition and, when everything a
// boot needs is in place, proposes the boot operation. hostName is the bound host.
func (r *HostClaimReconciler) provision(ctx context.Context, claim *metalv1alpha1.HostClaim, hostName string) error {
	set := func(s metav1.ConditionStatus, reason, msg string) {
		setCondition(&claim.Status.Conditions, claim.Generation, metalv1alpha1.ConditionProvisioned, s, reason, msg)
	}
	hold := func(reason, msg string) error {
		set(metav1.ConditionFalse, reason, msg)
		return nil
	}
	ops, err := r.claimOps(ctx, claim, purposeProvision)
	if err != nil {
		return err
	}
	if claim.Spec.OS == nil {
		// Intent dropped: every boot not finished goes with it — a proposal
		// nobody approved yet, and a boot still running, which stops between
		// steps. The node a claim boots changes only through here, so no boot
		// for the old node survives into the new intent.
		meta.RemoveStatusCondition(&claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
		return r.deleteOps(ctx, ops, unfinished, audit.DeleteStopped,
			"claim "+claim.Namespace+"/"+claim.Name+" no longer names an OS")
	}

	host := &metalv1alpha1.Host{}
	if err := r.Get(ctx, client.ObjectKey{Name: hostName}, host); err != nil {
		return client.IgnoreNotFound(err)
	}
	node := &osv1alpha1.TalosNode{}
	if err := r.Get(ctx, client.ObjectKey{Name: claim.Spec.OS.NodeRef.Name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return hold(metalv1alpha1.ReasonAwaitingRender, fmt.Sprintf("TalosNode %s not found", claim.Spec.OS.NodeRef.Name))
		}
		return err
	}
	// A TalosNode being deleted is taken out of its cluster, and its machine
	// wiped, while the claim keeps the machine (removeNode).
	if !node.DeletionTimestamp.IsZero() {
		return r.removeNode(ctx, claim, host, node, ops)
	}
	// The node ran for this claim on another machine before the claim was
	// moved (pk-metal replace): what its status says of a check-in came
	// from that machine, which the release has wiped since. This one is not
	// installed until a boot of its own says so.
	moved := claim.Status.HandOff != nil && claim.Status.HandOff.Node == node.Name && claim.Status.HandOff.Host != hostName
	if msg, ok := r.handedOff(claim, hostName, node, ops, moved); ok {
		set(metav1.ConditionTrue, metalv1alpha1.ReasonHandedOff, msg)
		if err := r.withdraw(ctx, ops, ""); err != nil {
			return err
		}
		return r.dropConfigMedia(ctx, node, ops)
	}
	withdrawAndHold := func(reason, msg string) error {
		if err := r.withdraw(ctx, ops, ""); err != nil {
			return err
		}
		return hold(reason, msg)
	}
	// A person said no for now: nothing new is proposed, and what still waits
	// for approval is taken back. An approved boot keeps running — stopping
	// one halfway is what dropping spec.os is for.
	if why, held := heldBy(claim); held {
		msg := fmt.Sprintf("held%s: no install is proposed while the claim carries the %s annotation. To go on: %s",
			why, metalv1alpha1.AnnotationHold, releaseHoldCommand(claim))
		for i := range ops {
			if opActive(&ops[i]) && ops[i].DeletionTimestamp.IsZero() {
				msg += fmt.Sprintf(". Boot operation %s/%s was approved before the hold and keeps running; to stop it, remove spec.os from the claim",
					ops[i].Namespace, ops[i].Name)
			}
		}
		return withdrawAndHold(metalv1alpha1.ReasonHeldByUser, msg)
	}
	// The same for a machine a person took into maintenance.
	if _, ok := inMaintenance(host); ok {
		return withdrawAndHold(metalv1alpha1.ReasonHostInMaintenance, maintenanceNote(host)+
			": no install is proposed until it is given back: "+endMaintenanceCommand(host))
	}
	if !meta.IsStatusConditionTrue(host.Status.Conditions, metalv1alpha1.ConditionVerified) {
		return hold(metalv1alpha1.ReasonAwaitingVerification, fmt.Sprintf("host %s is not verified", hostName))
	}
	// Another node's install left on the disk must not boot again, and is
	// not installed over without a person seeing it.
	if msg := notWipedHold(host, node.Spec.ClusterRef.Name, node.Name); msg != "" {
		return withdrawAndHold(metalv1alpha1.ReasonDiskNotWiped, msg)
	}
	// What the boot replaces: the same node (a reinstall), or another node
	// the machine ran for this claim before its OS intent was changed.
	var prior string
	if h := claim.Status.HandOff; h != nil && h.Host == hostName {
		prior = h.Node
	}

	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, client.ObjectKey{Name: node.Spec.ClusterRef.Name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return hold(metalv1alpha1.ReasonAwaitingRender, fmt.Sprintf("TalosCluster %s not found", node.Spec.ClusterRef.Name))
		}
		return err
	}
	if !meta.IsStatusConditionTrue(node.Status.Conditions, osv1alpha1.ConditionRendered) || node.Status.ConfigDigest == "" {
		return hold(metalv1alpha1.ReasonAwaitingRender, fmt.Sprintf("TalosNode %s has no rendered config yet", node.Name))
	}
	// A failed boot of this render holds the claim until a person retries
	// it. Its media are not made again for it: the ones that carry the
	// config are deleted, and the retry builds them anew.
	if failed := failedBoot(ops, hostName, node); failed != nil {
		if err := r.dropConfigMedia(ctx, node, ops); err != nil {
			return err
		}
		return hold(metalv1alpha1.ReasonOperationFailed, failedBootMessage(claim, failed))
	}

	plan, config, installer, reason, msg, err := r.installPlan(ctx, claim, host, node, cluster, prior, moved)
	if err != nil {
		return err
	}
	if reason != "" {
		return hold(reason, msg)
	}
	name := fmt.Sprintf("provision-%s-%s", hostName, hash8(planHash(plan)))
	if err := r.withdraw(ctx, ops, name); err != nil {
		return err
	}
	var current *metalv1alpha1.HostOperation
	for i := range ops {
		switch {
		case ops[i].Name == name:
			current = &ops[i]
		case opActive(&ops[i]) && ops[i].DeletionTimestamp.IsZero():
			// An older plan already running finishes first.
			return hold(metalv1alpha1.ReasonAwaitingOperation,
				fmt.Sprintf("operation %s/%s for an earlier plan is running", ops[i].Namespace, ops[i].Name))
		}
	}
	if current == nil {
		// An install reboots the machine. On one the operator runs on, that
		// waits for a person (SelfGuard).
		if msg, err := r.Self.Check(ctx, host); err != nil {
			return err
		} else if msg != "" {
			return hold(metalv1alpha1.ReasonSelfDisruption, "no install is proposed: "+msg)
		}
		// The node the boot replaces leaves its cluster first, in the same
		// operation; a new control plane waits for its turn to join.
		prefix, reason, msg, err := r.bootPrefix(ctx, claim, host, node, cluster, prior, name)
		if err != nil {
			return err
		}
		if reason != "" {
			return hold(reason, msg)
		}
		requesters, err := r.installRequesters(ctx, claim, node, cluster)
		if err != nil {
			return err
		}
		op := r.provisionOp(claim, hostName, name, plan, node, config, installer, requesters)
		op.Spec.Actions = append(prefix, op.Spec.Actions...)
		if prior != "" || moved {
			// Either way the node checked in before, from this machine or
			// the one it left: the boot waits for a check-in after it began.
			op.Annotations[annotationReinstallOf] = claim.Status.HandOff.At.UTC().Format(time.RFC3339)
			op.Labels[labelNode] = node.Name
		}
		if err := r.Create(ctx, op); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		logf.FromContext(ctx).Info("proposed boot operation", "operation", op.Namespace+"/"+op.Name,
			"host", hostName, "node", node.Name, "approvalRequired", *op.Spec.Approval.Required)
		if *op.Spec.Approval.Required {
			return hold(metalv1alpha1.ReasonAwaitingApproval, awaitsApprovalMessage(claim, op, "boot", "install"))
		}
		return hold(metalv1alpha1.ReasonAwaitingOperation, fmt.Sprintf("boot operation %s/%s proposed", op.Namespace, op.Name))
	}

	ex := meta.FindStatusCondition(current.Status.Conditions, metalv1alpha1.ConditionExecuted)
	ref := current.Namespace + "/" + current.Name
	switch {
	case (prior != "" || moved) && opSucceeded(current):
		// The replacing install is the hand-off now. Its new time makes a
		// reinstall annotation naming the old one ask for nothing.
		claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{
			Host: hostName, Node: node.Name, ConfigDigest: plan.ConfigDigest, At: metav1.Now(),
			Operation: current.Namespace + "/" + current.Name,
		}
		verb := "reinstalled"
		if moved {
			verb = "installed"
		}
		set(metav1.ConditionTrue, metalv1alpha1.ReasonHandedOff, fmt.Sprintf("operation %s %s host %s with TalosNode %s", ref, verb, hostName, node.Name))
		return r.dropConfigMedia(ctx, node, ops)
	case current.AwaitsApproval():
		return hold(metalv1alpha1.ReasonAwaitingApproval, awaitsApprovalMessage(claim, current, "boot", "install"))
	case ex != nil && ex.Status == metav1.ConditionFalse:
		if err := r.dropConfigMedia(ctx, node, ops); err != nil {
			return err
		}
		return hold(metalv1alpha1.ReasonOperationFailed, failedBootMessage(claim, current))
	case ex != nil && ex.Message != "":
		return hold(metalv1alpha1.ReasonAwaitingOperation, "boot operation "+ref+": "+ex.Message)
	}
	return hold(metalv1alpha1.ReasonAwaitingOperation, "boot operation "+ref+" in flight")
}

// handedOff says whether host hostName runs node for the claim already, with
// the message for Provisioned. It records a hand-off that only the node's
// check-in or a succeeded boot shows. A reinstall is not handed off, and a
// node that moved here (moved) is not handed off by its check-in: the boot
// that replaces the install decides.
func (r *HostClaimReconciler) handedOff(claim *metalv1alpha1.HostClaim, hostName string, node *osv1alpha1.TalosNode,
	ops []metalv1alpha1.HostOperation, moved bool) (string, bool) {
	if reinstallRequested(claim, hostName, node.Name) {
		return "", false
	}
	// The hand-off record on the claim says the install is done: the node may
	// simply be down now, and that is not a reason to install it again. A
	// record of any other host or node is no record of this one.
	if h := claim.Status.HandOff; h != nil && h.Host == hostName && h.Node == node.Name {
		at := h.At.UTC().Format(time.RFC3339)
		if !nodeCheckedIn(node) {
			return fmt.Sprintf("host %s was installed with TalosNode %s at %s; the node does not answer now, which does not start a new install",
				hostName, node.Name, at), true
		}
		return fmt.Sprintf("host %s runs TalosNode %s (installed %s)", hostName, node.Name, at), true
	}
	if nodeCheckedIn(node) && !moved {
		// The boot that got it there, if one of ours did: the release reads
		// it to know whether the operator touched this machine.
		var booted *metalv1alpha1.HostOperation
		for i := range ops {
			if opStarted(&ops[i]) && bootsNode(&ops[i], hostName, node.Name) {
				booted = &ops[i]
			}
		}
		recordHandOff(claim, hostName, node.Name, node.Status.ConfigDigest, booted)
		return fmt.Sprintf("host %s runs TalosNode %s", hostName, node.Name), true
	}
	// The boot succeeded, but the node does not answer now, and no pass
	// recorded its hand-off yet: the boot is the record.
	for i := range ops {
		if opSucceeded(&ops[i]) && bootsNode(&ops[i], hostName, node.Name) && ops[i].Annotations[annotationReinstallOf] == "" {
			recordHandOff(claim, hostName, node.Name, ops[i].Annotations[annotationConfigDigest], &ops[i])
			return fmt.Sprintf("operation %s/%s booted host %s", ops[i].Namespace, ops[i].Name, hostName), true
		}
	}
	return "", false
}

// installPlan picks how the install reaches the machine, makes the media it
// mounts and builds the plan from them. reason is empty when the plan is
// ready; otherwise reason and msg say what the install waits for.
func (r *HostClaimReconciler) installPlan(ctx context.Context, claim *metalv1alpha1.HostClaim, host *metalv1alpha1.Host,
	node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster, prior string, moved bool) (
	plan provisionPlan, config, installer *osv1alpha1.BootAsset, reason, msg string, err error) {
	hostName := host.Name
	shape, err := r.endpointShape(ctx, host)
	if err != nil {
		return plan, nil, nil, "", "", err
	}
	if len(shape.insertable) == 0 {
		return plan, nil, nil, metalv1alpha1.ReasonNoBootableMedia, fmt.Sprintf("host %s advertises no insertable media slot", hostName), nil
	}
	bootSlot, configSlot := shape.mediaSlots()
	// One slot cannot carry the installer and the config side by side. A
	// machine that already runs Talos from its disk (it answers, blank, in
	// maintenance mode) needs the config medium alone; a blank one gets an
	// installer with its config built in. Either way the config reaches the
	// machine through the BMC, never over the machine's own network.
	// A machine that replaces an install is reset first: it has no Talos on
	// disk to take a config medium alone.
	kind, why, msg := decideShape(hostName, shape,
		prior == "" && !moved && meta.IsStatusConditionTrue(node.Status.Conditions, osv1alpha1.ConditionMachineReachable),
		cluster.Spec.TalosVersion)
	if why != "" {
		return plan, nil, nil, why, msg, nil
	}

	plan = provisionPlan{ConfigDigest: node.Status.ConfigDigest}
	// A boot from the installer needs a boot target. Without one, no medium
	// is made: the config in it would wait for a boot that never comes.
	if kind != shapeConfigOnly {
		target, err := boot.PickTarget(shape.targets)
		if err != nil {
			return plan, nil, nil, metalv1alpha1.ReasonNoBootableMedia, err.Error(), nil
		}
		plan.Target, plan.Mode = target, overrideMode(shape.mode)
	}
	switch kind {
	case shapeEmbedded:
		if installer, err = r.ensureEmbeddedAsset(ctx, node, cluster); err != nil {
			return plan, nil, nil, "", "", err
		}
		if msg := embeddedAssetPending(installer, node, cluster); msg != "" {
			return plan, nil, nil, metalv1alpha1.ReasonAwaitingBootAsset, msg, nil
		}
		plan.ArtifactDigest, plan.Embedded = installer.Status.Digest, true
		plan.Slots = []string{shape.bootSlots[0]}
	default:
		if config, err = r.ensureConfigAsset(ctx, node); err != nil {
			return plan, nil, nil, "", "", err
		}
		if msg := configAssetPending(ctx, r.Client, config, node); msg != "" {
			return plan, nil, nil, metalv1alpha1.ReasonAwaitingBootAsset, msg, nil
		}
		plan.ArtifactDigest = config.Status.Digest
		if kind == shapeConfigOnly {
			plan.Slots = shape.insertable[:1]
			break
		}
		if installer, err = r.ensureInstallerAsset(ctx, node, cluster); err != nil {
			return plan, nil, nil, "", "", err
		}
		if msg := installerPending(installer); msg != "" {
			return plan, nil, nil, metalv1alpha1.ReasonAwaitingBootAsset, msg, nil
		}
		plan.Installer, plan.InstallerDigest = installer.Status.URL, installer.Status.Digest
		plan.Slots = []string{bootSlot, configSlot}
	}

	if prior != "" {
		plan.Replaces = prior + "@" + claim.Status.HandOff.At.UTC().Format(time.RFC3339)
	}
	return plan, config, installer, "", "", nil
}

// failedBoot is the failed boot of node's current render onto the host,
// when nothing else for the host is in flight; nil otherwise. A boot that
// started fails only by its own steps, and only a person starts it again
// (pk-metal retry deletes it).
func failedBoot(ops []metalv1alpha1.HostOperation, hostName string, node *osv1alpha1.TalosNode) *metalv1alpha1.HostOperation {
	var failed *metalv1alpha1.HostOperation
	for i := range ops {
		op := &ops[i]
		if !op.DeletionTimestamp.IsZero() {
			continue
		}
		if !op.Finished() {
			return nil
		}
		if !opSucceeded(op) && bootsNode(op, hostName, node.Name) &&
			op.Annotations[annotationConfigDigest] == node.Status.ConfigDigest {
			failed = op
		}
	}
	return failed
}

func failedBootMessage(claim *metalv1alpha1.HostClaim, op *metalv1alpha1.HostOperation) string {
	ref := op.Namespace + "/" + op.Name
	var why string
	if ex := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted); ex != nil {
		why = ex.Message
	}
	return fmt.Sprintf("boot operation %s failed: %s. To try again: pk-metal retry %s. To stop trying: pk-metal hold %s/%s",
		ref, why, ref, claim.Namespace, claim.Name)
}

// dropConfigMedia deletes the boot media the lane made for node that carry
// its machine config, once no boot of the claim runs: the config ISO (its
// artifact Secret goes with it, as its owner) and the installer with the
// config built in (the store sweep deletes its file). After the install, or
// after a failed boot, nothing mounts them again; the operations keep the
// digest of each medium they mounted, which is the record. A later boot of
// the node makes them anew. A medium a person made is left alone.
func (r *HostClaimReconciler) dropConfigMedia(ctx context.Context, node *osv1alpha1.TalosNode, ops []metalv1alpha1.HostOperation) error {
	for i := range ops {
		if !ops[i].Finished() && ops[i].DeletionTimestamp.IsZero() {
			return nil
		}
	}
	for _, name := range []string{node.Name + "-config", embeddedName(node.Name)} {
		a := &osv1alpha1.BootAsset{}
		if err := r.Get(ctx, client.ObjectKey{Name: name}, a); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if a.Labels[labelProvisioning] != "true" || !a.DeletionTimestamp.IsZero() || !secretBearing(a) ||
			a.Spec.NodeRef == nil || a.Spec.NodeRef.Name != node.Name {
			continue
		}
		if err := r.Delete(ctx, a); client.IgnoreNotFound(err) != nil {
			return err
		}
		logf.FromContext(ctx).Info("deleted a boot medium that carries the machine config", "bootAsset", a.Name,
			"node", node.Name, "digest", a.Status.Digest)
	}
	return nil
}

// heldBy reports whether the claim carries the hold annotation, with its
// reason shaped for a message: " (reason)", or "" when none was given.
func heldBy(claim *metalv1alpha1.HostClaim) (string, bool) {
	v, ok := claim.Annotations[metalv1alpha1.AnnotationHold]
	if !ok {
		return "", false
	}
	if v = strings.TrimSpace(v); v != "" {
		return " (" + v + ")", true
	}
	return "", true
}

// releaseHoldCommand is what a person runs to lift the hold.
func releaseHoldCommand(claim *metalv1alpha1.HostClaim) string {
	return fmt.Sprintf("kubectl annotate hostclaim -n %s %s %s-", claim.Namespace, claim.Name, metalv1alpha1.AnnotationHold)
}

// awaitsApprovalMessage tells a person what waits, and the commands that say
// yes or no to it.
func awaitsApprovalMessage(claim *metalv1alpha1.HostClaim, op *metalv1alpha1.HostOperation, kind, verb string) string {
	ref := op.Namespace + "/" + op.Name
	return fmt.Sprintf("%s operation %s awaits approval. To %s: pk-metal approve %s. To say no for now: pk-metal hold %s/%s",
		kind, ref, verb, ref, claim.Namespace, claim.Name)
}

// recordHandOff writes the hand-off record onto the claim, once: a record
// for this host and node already there keeps its time.
// booted is the boot operation that installed it, nil when the machine was
// found already running the node.
func recordHandOff(claim *metalv1alpha1.HostClaim, hostName, nodeName, digest string, booted *metalv1alpha1.HostOperation) {
	if h := claim.Status.HandOff; h != nil && h.Host == hostName && h.Node == nodeName {
		return
	}
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{
		Host: hostName, Node: nodeName, ConfigDigest: digest, At: metav1.Now(),
	}
	if booted != nil {
		claim.Status.HandOff.Operation = booted.Namespace + "/" + booted.Name
	}
}

// overrideMode is the override mode a boot asks for, given the one the system
// serves. The media boot UEFI: a system serving another mode would take the
// override as a legacy boot, so the mode goes along; one serving UEFI already
// needs nothing, and one serving none is not asked for it (it may refuse).
func overrideMode(served string) string {
	if served != "" && served != "UEFI" {
		return "UEFI"
	}
	return ""
}

// bootsNode: the operation acts on the host and waits for the node to check in.
func bootsNode(op *metalv1alpha1.HostOperation, hostName, nodeName string) bool {
	if op.Spec.HostRef.Name != hostName {
		return false
	}
	return slices.ContainsFunc(op.Spec.Actions, func(a metalv1alpha1.HostOperationAction) bool {
		return a.AwaitCheckIn != nil && a.AwaitCheckIn.NodeRef.Name == nodeName
	})
}

// installRequesters names who asked for what an install puts on the
// machine: whoever last changed the claim (a reinstall mark, a new
// machine), the node, its cluster or a patch ConfigMap either reads. The
// install applies their edits, as a rollout of the cluster would.
func (r *HostClaimReconciler) installRequesters(ctx context.Context, claim *metalv1alpha1.HostClaim,
	node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster) ([]string, error) {
	names, err := clusterRequesters(ctx, r.Client, r.CredentialsNamespace, cluster, []osv1alpha1.TalosNode{*node})
	if err != nil {
		return nil, err
	}
	names = append(names, metalv1alpha1.RequestedBy(claim.Annotations)...)
	slices.Sort(names)
	return slices.Compact(names), nil
}

// provisionOp builds the boot operation for a plan. config is nil for an
// embedded plan, installer is nil for a config-only one. Each medium is
// pinned to the digest the plan was made from.
func (r *HostClaimReconciler) provisionOp(claim *metalv1alpha1.HostClaim, hostName, name string, plan provisionPlan,
	node *osv1alpha1.TalosNode, config, installer *osv1alpha1.BootAsset, requestedBy []string) *metalv1alpha1.HostOperation {
	var actions []metalv1alpha1.HostOperationAction
	for _, s := range plan.Slots {
		actions = append(actions, metalv1alpha1.HostOperationAction{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: s}})
	}
	insert := func(slot string, asset *osv1alpha1.BootAsset, digest string) {
		actions = append(actions, metalv1alpha1.HostOperationAction{InsertMedia: &metalv1alpha1.InsertMediaAction{
			MediaID: slot, BootAssetRef: &metalv1alpha1.Reference{Name: asset.Name}, Digest: digest,
		}})
	}
	switch {
	case plan.Embedded:
		insert(plan.Slots[0], installer, plan.ArtifactDigest)
	default:
		if installer != nil {
			insert(plan.Slots[0], installer, plan.InstallerDigest)
		}
		insert(plan.Slots[len(plan.Slots)-1], config, plan.ArtifactDigest)
	}
	if plan.Target != "" {
		actions = append(actions, metalv1alpha1.HostOperationAction{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{
			Target: plan.Target, Once: ptr.To(true), Mode: plan.Mode,
		}})
	}
	actions = append(actions,
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
		metalv1alpha1.HostOperationAction{AwaitCheckIn: &metalv1alpha1.AwaitCheckInAction{
			NodeRef: metalv1alpha1.Reference{Name: node.Name}, TimeoutSeconds: r.checkInTimeoutSeconds(),
		}},
	)
	// The media leave once the machine runs from disk: config first, it is
	// the one carrying secrets.
	for i := len(plan.Slots) - 1; i >= 0; i-- {
		actions = append(actions, metalv1alpha1.HostOperationAction{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: plan.Slots[i]}})
	}
	required := !approvalWaived(claim)
	return &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: r.OperationsNamespace,
			Labels:      claimLabels(claim, purposeProvision),
			Annotations: map[string]string{annotationConfigDigest: plan.ConfigDigest},
		},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:     metalv1alpha1.Reference{Name: hostName},
			ProposedBy:  r.Proposer,
			RequestedBy: requestedBy,
			Reason:      reasonProvisioning,
			PlanHash:    planHash(plan),
			Actions:     actions,
			Approval:    metalv1alpha1.OperationApproval{Required: ptr.To(required)},
		},
	}
}

func (r *HostClaimReconciler) checkInTimeoutSeconds() int32 {
	if r.CheckInTimeoutSeconds > 0 {
		return r.CheckInTimeoutSeconds
	}
	return int32(defaultCheckInTimeout.Seconds())
}

func claimLabels(claim *metalv1alpha1.HostClaim, purpose string) map[string]string {
	return map[string]string{
		labelClaimNamespace: claim.Namespace,
		labelClaimName:      claim.Name,
		labelClaimUID:       string(claim.UID),
		labelPurpose:        purpose,
	}
}

// claimOps lists the operator's operations serving this claim for one purpose.
func (r *HostClaimReconciler) claimOps(ctx context.Context, claim *metalv1alpha1.HostClaim, purpose string) ([]metalv1alpha1.HostOperation, error) {
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.List(ctx, ops, client.InNamespace(r.OperationsNamespace), client.MatchingLabels{
		labelClaimUID: string(claim.UID), labelPurpose: purpose,
	}); err != nil {
		return nil, err
	}
	return ops.Items, nil
}

// withdraw retracts the claim's proposals that never got their gate, except
// keep — a superseded plan is not left in front of the approver.
func (r *HostClaimReconciler) withdraw(ctx context.Context, ops []metalv1alpha1.HostOperation, keep string) error {
	return r.deleteOps(ctx, ops, func(op *metalv1alpha1.HostOperation) bool { return op.Name != keep && op.AwaitsApproval() },
		audit.DeleteWithdrawn, "a newer boot plan replaces it before anyone approved it")
}

// deleteOps deletes each operation of ops that pick selects (each one when
// pick is nil) and that is not being deleted yet, with a line in the manager
// log and in the audit trail. An operation that is gone is no error.
func (r *HostClaimReconciler) deleteOps(ctx context.Context, ops []metalv1alpha1.HostOperation,
	pick func(*metalv1alpha1.HostOperation) bool, reason audit.DeleteReason, why string) error {
	for i := range ops {
		op := &ops[i]
		if !op.DeletionTimestamp.IsZero() || (pick != nil && !pick(op)) {
			continue
		}
		logf.FromContext(ctx).Info("deleted operation", "operation", op.Namespace+"/"+op.Name, "reason", reason, "why", why)
		if err := deleteOp(ctx, r.Client, r.Audit, op, reason, why); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// unfinished: the operation has not ended. Deleted, a running one stops
// between steps.
func unfinished(op *metalv1alpha1.HostOperation) bool { return !op.Finished() }

// findOp is the operation of ops with that name, or nil.
func findOp(ops []metalv1alpha1.HostOperation, name string) *metalv1alpha1.HostOperation {
	for i := range ops {
		if ops[i].Name == name {
			return &ops[i]
		}
	}
	return nil
}

// endpointView is what a boot plan reads off the host's endpoint.
type endpointView struct {
	// insertable lists every slot with an insert mechanism, best boot slot
	// first (bmc.InsertableSlots) — what a release empties.
	insertable []string
	// bootSlots are the insertable slots an ISO boots from: optical ones,
	// then untyped ones (a slot that advertises nothing may still be one),
	// in the endpoint's order. A slot typed as something else (a USB stick)
	// is left out: the installer boots from one of these, the config medium
	// may take any other insertable slot.
	bootSlots []string
	// targets are the boot override targets the path's system allows.
	targets []string
	// mode is the override mode the path's system serves.
	mode string
	// known: the slots were read from an inventoried endpoint on a
	// reachable path. Without that, an empty insertable list means "not
	// read yet", not "no slots".
	known bool
}

// endpointShape reads the media slots and the boot facts of the host's first
// reachable path.
func (r *HostClaimReconciler) endpointShape(ctx context.Context, host *metalv1alpha1.Host) (endpointView, error) {
	for _, path := range host.Status.ManagedBy {
		if !path.Reachable {
			continue
		}
		ep := &metalv1alpha1.ManagementEndpoint{}
		if err := r.Get(ctx, client.ObjectKey{Name: path.EndpointRef.Name}, ep); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return endpointView{}, err
		}
		v := endpointView{known: meta.IsStatusConditionTrue(ep.Status.Conditions, metalv1alpha1.ConditionInventoried)}
		caps := bmc.Capabilities{MediaDialect: map[string]bmc.MediaDialect{}}
		if c := ep.Status.Capabilities; c != nil {
			for id, d := range c.MediaDialects {
				caps.MediaDialect[id] = bmc.MediaDialect(d)
			}
		}
		media := make([]bmc.VirtualMedia, 0, len(ep.Status.VirtualMedia))
		types := map[string][]string{}
		for _, m := range ep.Status.VirtualMedia {
			media = append(media, bmc.VirtualMedia{ID: m.ID, MediaTypes: m.MediaTypes})
			types[m.ID] = m.MediaTypes
		}
		v.insertable = bmc.InsertableSlots(media, caps)
		for _, id := range v.insertable {
			if t := types[id]; len(t) == 0 || slices.Contains(t, "CD") || slices.Contains(t, "DVD") {
				v.bootSlots = append(v.bootSlots, id)
			}
		}
		for _, sys := range ep.Status.Systems {
			if sys.ID == path.SystemID {
				v.targets, v.mode = sys.BootTargets, sys.BootOverrideMode
			}
		}
		return v, nil
	}
	return endpointView{}, nil
}

// mediaSlots picks the slots of a two-medium boot: the installer goes on the
// best boot slot, the config medium on the next insertable slot. The config
// medium is only read, never booted: Talos finds it by its volume label on any
// block device, so a USB-stick slot carries it as well as an optical one. Both
// are empty when the endpoint cannot hold an installer next to a config.
func (v endpointView) mediaSlots() (boot, config string) {
	if len(v.bootSlots) == 0 {
		return "", ""
	}
	for _, id := range v.insertable {
		if id != v.bootSlots[0] {
			return v.bootSlots[0], id
		}
	}
	return "", ""
}

// ensureConfigAsset finds the node's configISO BootAsset, creating one owned
// by the node when nobody authored it.
func (r *HostClaimReconciler) ensureConfigAsset(ctx context.Context, node *osv1alpha1.TalosNode) (*osv1alpha1.BootAsset, error) {
	assets := &osv1alpha1.BootAssetList{}
	if err := r.List(ctx, assets); err != nil {
		return nil, err
	}
	for i := range assets.Items {
		a := &assets.Items[i]
		if a.Spec.Strategy == osv1alpha1.StrategyConfigISO && a.Spec.NodeRef != nil && a.Spec.NodeRef.Name == node.Name &&
			a.DeletionTimestamp.IsZero() {
			return a, nil
		}
	}
	a := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-config", Labels: map[string]string{labelProvisioning: "true"}},
		Spec: osv1alpha1.BootAssetSpec{
			Strategy: osv1alpha1.StrategyConfigISO,
			NodeRef:  &osv1alpha1.Reference{Name: node.Name},
		},
	}
	if err := controllerutil.SetOwnerReference(node, a, r.Scheme()); err != nil {
		return nil, err
	}
	return a, r.createOrGet(ctx, a)
}

// bootSchematic is the schematic of the boot medium: the node's effective
// schematic plus the kernel argument that makes Talos read its configuration
// from the metal-iso volume. The argument belongs to the boot medium only; the
// installed system takes its command line from the installer image, which the
// rendered machine configuration names, so it never looks for the medium again.
func bootSchematic(node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster) *osv1alpha1.Schematic {
	s := &osv1alpha1.Schematic{}
	if eff := render.EffectiveSchematic(cluster, node); eff != nil {
		s = eff.DeepCopy()
	}
	if !slices.Contains(s.Customization.ExtraKernelArgs, bootConfigKernelArg) {
		s.Customization.ExtraKernelArgs = append(s.Customization.ExtraKernelArgs, bootConfigKernelArg)
	}
	return s
}

// bootConfigKernelArg points Talos at the config ISO the operator mounts.
const bootConfigKernelArg = "talos.config=metal-iso"

// ensureInstallerAsset finds a factory BootAsset for the node's boot schematic
// and the cluster's Talos version, creating one owned by the cluster when absent.
// The factory strategy is what registers the schematic and proves the image
// exists before anyone mounts its URL.
//
// Each cluster gets its own: an asset owned by one cluster is collected with
// it, and a boot of another cluster's node that shared it would lose its
// image halfway. An asset nobody owns (authored by a person) is shared freely.
func (r *HostClaimReconciler) ensureInstallerAsset(ctx context.Context, node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster) (*osv1alpha1.BootAsset, error) {
	schematic := bootSchematic(node, cluster)
	assets := &osv1alpha1.BootAssetList{}
	if err := r.List(ctx, assets); err != nil {
		return nil, err
	}
	for i := range assets.Items {
		a := &assets.Items[i]
		if a.Spec.Strategy == osv1alpha1.StrategyFactory && a.Spec.TalosVersion == cluster.Spec.TalosVersion &&
			equality.Semantic.DeepEqual(a.Spec.Schematic, schematic) && a.DeletionTimestamp.IsZero() &&
			(len(a.OwnerReferences) == 0 || ownedBy(a, cluster)) {
			return a, nil
		}
	}
	a := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{
			Name:   installerName(cluster.Name, hash8(planHash([]any{schematic, cluster.Spec.TalosVersion}))),
			Labels: map[string]string{labelProvisioning: "true"},
		},
		Spec: osv1alpha1.BootAssetSpec{
			Strategy:     osv1alpha1.StrategyFactory,
			TalosVersion: cluster.Spec.TalosVersion,
			Schematic:    schematic,
		},
	}
	if err := controllerutil.SetOwnerReference(cluster, a, r.Scheme()); err != nil {
		return nil, err
	}
	return a, r.createOrGet(ctx, a)
}

// ensureEmbeddedAsset finds the node's embeddedInstaller BootAsset, creating
// one owned by the node when nobody authored it. One the lane created follows
// the cluster's Talos version and the node's schematic; one a person authored
// is used as it is.
func (r *HostClaimReconciler) ensureEmbeddedAsset(ctx context.Context, node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster) (*osv1alpha1.BootAsset, error) {
	want := osv1alpha1.BootAssetSpec{
		Strategy:     osv1alpha1.StrategyEmbeddedInstaller,
		NodeRef:      &osv1alpha1.Reference{Name: node.Name},
		TalosVersion: cluster.Spec.TalosVersion,
		Schematic:    render.EffectiveSchematic(cluster, node),
	}
	assets := &osv1alpha1.BootAssetList{}
	if err := r.List(ctx, assets); err != nil {
		return nil, err
	}
	for i := range assets.Items {
		a := &assets.Items[i]
		if a.Spec.Strategy != osv1alpha1.StrategyEmbeddedInstaller || a.Spec.NodeRef == nil || a.Spec.NodeRef.Name != node.Name ||
			!a.DeletionTimestamp.IsZero() {
			continue
		}
		if a.Labels[labelProvisioning] == "true" && !equality.Semantic.DeepEqual(a.Spec, want) {
			a.Spec = want
			if err := r.Update(ctx, a); err != nil {
				return nil, err
			}
			logf.FromContext(ctx).Info("updated embedded installer inputs", "bootAsset", a.Name, "talosVersion", want.TalosVersion)
		}
		return a, nil
	}
	a := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: embeddedName(node.Name), Labels: map[string]string{labelProvisioning: "true"}},
		Spec:       want,
	}
	if err := controllerutil.SetOwnerReference(node, a, r.Scheme()); err != nil {
		return nil, err
	}
	return a, r.createOrGet(ctx, a)
}

// embeddedName is <node>-embedded, the node part cut so the name stays a
// valid object name.
func embeddedName(node string) string {
	const maxPrefix = 253 - len("-embedded")
	if len(node) > maxPrefix {
		node = strings.TrimRight(node[:maxPrefix], "-.")
	}
	return node + "-embedded"
}

// installerPending says why the factory installer cannot go into a plan
// yet: the operator has not downloaded it into the media store. Empty when
// it is there.
func installerPending(a *osv1alpha1.BootAsset) string {
	c := meta.FindStatusCondition(a.Status.Conditions, osv1alpha1.ConditionReady)
	switch {
	case c == nil || a.Status.Digest == "" || a.Status.BuildKey == "":
		return fmt.Sprintf("installer image %s is not in the media store yet", a.Name)
	case c.Status != metav1.ConditionTrue:
		return fmt.Sprintf("installer image %s is not ready (%s): %s", a.Name, c.Reason, c.Message)
	}
	return ""
}

// embeddedAssetPending says why the embedded installer cannot be mounted yet,
// or "" when it is built from the node's current render and the cluster's
// Talos version.
func embeddedAssetPending(a *osv1alpha1.BootAsset, node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster) string {
	c := meta.FindStatusCondition(a.Status.Conditions, osv1alpha1.ConditionReady)
	switch {
	case c == nil:
		return fmt.Sprintf("installer %s with the node's config built in is not built yet", a.Name)
	case c.ObservedGeneration != a.Generation:
		return fmt.Sprintf("installer %s with the node's config built in: inputs changed, build not started yet", a.Name)
	case c.Status != metav1.ConditionTrue:
		return fmt.Sprintf("installer %s with the node's config built in is not ready (%s): %s", a.Name, c.Reason, c.Message)
	case a.Status.Digest == "" || a.Status.BuildKey == "":
		return fmt.Sprintf("installer %s with the node's config built in is not built yet", a.Name)
	case a.Status.ConfigDigest != node.Status.ConfigDigest:
		return fmt.Sprintf("installer %s carries render %s, node is at %s", a.Name, short(a.Status.ConfigDigest), short(node.Status.ConfigDigest))
	case a.Spec.TalosVersion != cluster.Spec.TalosVersion:
		return fmt.Sprintf("installer %s is Talos %s, the cluster runs %s", a.Name, a.Spec.TalosVersion, cluster.Spec.TalosVersion)
	}
	return ""
}

// installerName is <cluster>-installer-<hash>, the cluster part cut so the
// name stays a valid object name.
func installerName(cluster, hash string) string {
	const maxPrefix = 253 - len("-installer-") - 8
	if len(cluster) > maxPrefix {
		cluster = strings.TrimRight(cluster[:maxPrefix], "-.")
	}
	return cluster + "-installer-" + hash
}

// ownedBy: one of the object's owner references is the owner.
func ownedBy(obj metav1.Object, owner metav1.Object) bool {
	return slices.ContainsFunc(obj.GetOwnerReferences(), func(ref metav1.OwnerReference) bool {
		return ref.UID == owner.GetUID()
	})
}

func (r *HostClaimReconciler) createOrGet(ctx context.Context, a *osv1alpha1.BootAsset) error {
	err := r.Create(ctx, a)
	if apierrors.IsAlreadyExists(err) {
		return r.Get(ctx, client.ObjectKeyFromObject(a), a)
	}
	if err == nil {
		logf.FromContext(ctx).Info("created boot asset", "bootAsset", a.Name, "strategy", a.Spec.Strategy)
	}
	return err
}

// configAssetPending says why the config medium cannot be mounted yet, or ""
// when it is built from the node's current render.
func configAssetPending(ctx context.Context, c client.Client, a *osv1alpha1.BootAsset, node *osv1alpha1.TalosNode) string {
	if !meta.IsStatusConditionTrue(a.Status.Conditions, osv1alpha1.ConditionReady) || a.Status.Digest == "" ||
		a.Status.ArtifactSecretRef == nil {
		return fmt.Sprintf("config medium %s is not built yet", a.Name)
	}
	ref := a.Status.ArtifactSecretRef
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, sec); err != nil {
		return fmt.Sprintf("config medium %s: artifact %s/%s unreadable: %v", a.Name, ref.Namespace, ref.Name, err)
	}
	if got := sec.Annotations[configDigestAnnotation]; got != node.Status.ConfigDigest {
		return fmt.Sprintf("config medium %s holds render %s, node is at %s", a.Name, short(got), short(node.Status.ConfigDigest))
	}
	return ""
}

func short(digest string) string {
	if d := strings.TrimPrefix(digest, "sha256:"); len(d) >= 8 {
		return d[:8]
	}
	return digest
}

// release hands the claim's machines back: a machine a boot operation of the
// claim actually ran on is powered off and its media ejected by an
// operator-created operation, unfinished boot operations are withdrawn, and
// only then are the bind and the hand-off cleared. A machine no boot ever
// touched is unbound as it is — powering it off would be the first thing the
// operator ever did to it. Returns whether everything is released. Wiping the
// disks is not part of it.
func (r *HostClaimReconciler) release(ctx context.Context, claim *metalv1alpha1.HostClaim, bound []metalv1alpha1.Host) (bool, error) {
	set := func(reason, msg string) {
		setCondition(&claim.Status.Conditions, claim.Generation, metalv1alpha1.ConditionProvisioned, metav1.ConditionFalse, reason, msg)
	}
	provisionOps, err := r.claimOps(ctx, claim, purposeProvision)
	if err != nil {
		return false, err
	}
	releaseOps, err := r.claimOps(ctx, claim, purposeRelease)
	if err != nil {
		return false, err
	}

	// pk-metal replace releases the old machine of a claim that stays.
	cause := "is deleted"
	if claim.DeletionTimestamp.IsZero() {
		cause = "moves to another machine"
	}
	why, held := heldBy(claim)
	keepRecords := held
	done := true
	for i := range bound {
		host := &bound[i]
		if ref := host.Spec.ClaimRef; ref != nil && ref.UID != "" && ref.UID != claim.UID {
			continue
		}
		name := fmt.Sprintf("release-%s-%s", host.Name, short8(string(claim.UID)))
		op := findOp(releaseOps, name)
		existed := op != nil
		// The decision to power off is made once, while the boot records are
		// still there to read; the release operation then carries it.
		// A node found already running when the claim was made counts too:
		// it is a node of a cluster this operator renders, and releasing its
		// machine takes it out of that cluster. So does a node the Host
		// reports running: its OSReady and the claim's hand-off record follow
		// the same check-in from two controllers, and a claim deleted between
		// the two writes would otherwise unbind a running node as it is.
		powerOff := op != nil || runsNode(host) || bootRan(provisionOps, host.Name) ||
			installedOnRecord(claim, host.Name, provisionOps) || powerOffDecided(host, claim)
		if !powerOff {
			// Unbinding as it is cannot be taken back: confirm against the
			// API server that no boot of the claim ran.
			if powerOff, err = r.bootRanUncached(ctx, claim, host.Name, "",
				"claim "+claim.Namespace+"/"+claim.Name+" releases host "+host.Name+" before this boot started"); err != nil {
				return false, err
			}
		}
		if powerOff {
			// A hold stops the release before it is approved: the proposal is
			// taken back, and none is made. One already running finishes.
			if held && (op == nil || op.AwaitsApproval()) {
				if op != nil {
					logf.FromContext(ctx).Info("withdrew release proposal of a held claim", "operation", op.Namespace+"/"+op.Name)
					if err := deleteOp(ctx, r.Client, r.Audit, op, audit.DeleteWithdrawn,
						"claim "+claim.Namespace+"/"+claim.Name+" is held before anyone approved its release"); client.IgnoreNotFound(err) != nil {
						return false, err
					}
				}
				set(metalv1alpha1.ReasonHeldByUser, fmt.Sprintf(
					"held%s: the claim %s, but host %s is neither shut down nor released while the claim carries the %s annotation. To go on: %s",
					why, cause, host.Name, metalv1alpha1.AnnotationHold, releaseHoldCommand(claim)))
				done = false
				continue
			}
			if _, maint := inMaintenance(host); maint && op == nil {
				set(metalv1alpha1.ReasonHostInMaintenance, maintenanceNote(host)+
					": the claim "+cause+", but the machine is shut down and released only after it is given back: "+endMaintenanceCommand(host))
				done, keepRecords = false, true
				continue
			}
			if op == nil {
				// The release powers the machine off. On one the operator
				// runs on, that waits for a person (SelfGuard).
				if msg, err := r.Self.Check(ctx, host); err != nil {
					return false, err
				} else if msg != "" {
					if err := r.recordPowerOff(ctx, host, claim, false); err != nil {
						return false, err
					}
					set(metalv1alpha1.ReasonSelfDisruption, "the claim "+cause+", but the machine is not shut down: "+msg)
					done, keepRecords = false, true
					continue
				}
				var reason, waiting string
				if op, reason, waiting, err = r.releaseOp(ctx, claim, host, name, cause, provisionOps); err != nil {
					return false, err
				}
				if op == nil {
					// The decision stands while the release waits.
					if err := r.recordPowerOff(ctx, host, claim, false); err != nil {
						return false, err
					}
					set(reason, waiting)
					done = false
					continue
				}
			}
			// Record the decision, and what the release cannot wipe, before
			// the boot records it came from go.
			marked := false
			if v := op.Annotations[metalv1alpha1.AnnotationDiskNotWiped]; v != "" {
				cluster, node, _ := strings.Cut(v, "/")
				marked = markNotWiped(host, cluster, node)
			}
			if err := r.recordPowerOff(ctx, host, claim, marked); err != nil {
				return false, err
			}
			if !existed {
				if err := r.Create(ctx, op); err != nil && !apierrors.IsAlreadyExists(err) {
					return false, err
				}
				logf.FromContext(ctx).Info("proposed release operation", "operation", op.Namespace+"/"+op.Name,
					"host", host.Name, "approvalRequired", *op.Spec.Approval.Required)
			}
			ex := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
			switch {
			case op.AwaitsApproval():
				set(metalv1alpha1.ReasonReleasing, fmt.Sprintf(
					"host %s runs or ran its node, so releasing it waits for a person: the release %s. To go ahead: pk-metal approve %s/%s. "+
						"To keep the machine running for now: pk-metal hold %s/%s",
					host.Name, describeRelease(op), op.Namespace, op.Name, claim.Namespace, claim.Name))
				done = false
				continue
			case ex != nil && ex.Status == metav1.ConditionFalse:
				set(metalv1alpha1.ReasonReleaseFailed, fmt.Sprintf(
					"release operation %s/%s failed: %s. To try again: pk-metal retry %s/%s",
					op.Namespace, op.Name, ex.Message, op.Namespace, op.Name))
				done = false
				continue
			case ex == nil || ex.Status != metav1.ConditionTrue:
				set(metalv1alpha1.ReasonReleasing, fmt.Sprintf("powering off host %s (operation %s/%s)", host.Name, op.Namespace, op.Name))
				done = false
				continue
			}
		}
		if op != nil && opSucceeded(op) && wipesDisk(op) {
			// The reset wiped the machine: whatever an earlier release
			// could not wipe is gone too.
			delete(host.Annotations, metalv1alpha1.AnnotationDiskNotWiped)
		}
		if err := r.releaseAll(ctx, []metalv1alpha1.Host{*host}); err != nil {
			return false, err
		}
	}
	// A held claim, or one whose host is in maintenance, keeps its boot
	// records: they are what the release reads once the stop is lifted.
	if keepRecords && !done {
		return false, nil
	}
	// Running boot operations stop between steps once deleted; their grants
	// die with them. They go only after the release operations exist, so a
	// failed pass never loses the record that a boot ran.
	if err := r.deleteOps(ctx, provisionOps, unfinished, audit.DeleteStopped,
		"claim "+claim.Namespace+"/"+claim.Name+" "+cause); err != nil {
		return false, err
	}
	if !done {
		return false, nil
	}
	// The boot records belong to a machine that the claim gave back. They
	// are the claim's working state, not only its history, so they go with
	// the machine and are not kept for the site's retention; the audit trail
	// keeps what they decided.
	if err := r.deleteOps(ctx, provisionOps, nil, audit.DeleteRecordOfRemoved,
		"claim "+claim.Namespace+"/"+claim.Name+" "+cause+"; the machine it held is released"); err != nil {
		return false, err
	}
	return true, nil
}

// bootRan: a boot operation of the claim was taken up by the executor on the
// host — it captured its baseline, the record it writes before its first
// mutating call.
func bootRan(ops []metalv1alpha1.HostOperation, hostName string) bool {
	return slices.ContainsFunc(ops, func(op metalv1alpha1.HostOperation) bool {
		return op.Spec.HostRef.Name == hostName && opStarted(&op)
	})
}

// bootRanUncached is bootRan over the claim's boot operations as the API
// server holds them, only those of nodeName when it is not "". The executor
// writes the baseline before its first mutating call, and the cache can lag
// behind that write.
//
// When no boot ran, it also deletes the unfinished boot operations it looked
// at, each only at the version it just read, with why in the audit trail.
// That fences the executor: an operation that captured its baseline since
// the read has a newer version, so its delete fails and the pass is retried,
// and one deleted first can no longer write a baseline, so it never reaches
// a step that touches the BMC.
//
//pk:uncached
func (r *HostClaimReconciler) bootRanUncached(ctx context.Context, claim *metalv1alpha1.HostClaim, hostName, nodeName, why string) (bool, error) {
	list := &metalv1alpha1.HostOperationList{}
	if err := r.reader().List(ctx, list, client.InNamespace(r.OperationsNamespace), client.MatchingLabels{
		labelClaimUID: string(claim.UID), labelPurpose: purposeProvision,
	}); err != nil {
		return false, err
	}
	ops := slices.DeleteFunc(list.Items, func(op metalv1alpha1.HostOperation) bool {
		return nodeName != "" && !bootsNode(&op, hostName, nodeName)
	})
	if bootRan(ops, hostName) {
		return true, nil
	}
	for i := range ops {
		op := &ops[i]
		if op.Spec.HostRef.Name != hostName || op.Finished() {
			continue
		}
		uid, rv := op.UID, op.ResourceVersion
		err := deleteOp(ctx, r.Client, r.Audit, op, audit.DeleteStopped, why,
			client.Preconditions{UID: &uid, ResourceVersion: &rv})
		if apierrors.IsConflict(err) {
			return false, fmt.Errorf("boot operation %s/%s changed since it was read; reading it again: %w",
				op.Namespace, op.Name, err)
		}
		if client.IgnoreNotFound(err) != nil {
			return false, err
		}
	}
	return false, nil
}

// powerOffDecided: an earlier pass decided that releasing the host for this
// claim powers it off.
func powerOffDecided(host *metalv1alpha1.Host, claim *metalv1alpha1.HostClaim) bool {
	return claim.UID != "" && host.Annotations[annotationReleasePowerOff] == string(claim.UID)
}

// recordPowerOff writes the power-off decision onto the host, with any mark
// the caller set on it in memory (markNotWiped). releaseAll removes the
// decision when the host is unbound; the mark stays.
func (r *HostClaimReconciler) recordPowerOff(ctx context.Context, host *metalv1alpha1.Host, claim *metalv1alpha1.HostClaim, marked bool) error {
	if powerOffDecided(host, claim) && !marked {
		return nil
	}
	if host.Annotations == nil {
		host.Annotations = map[string]string{}
	}
	host.Annotations[annotationReleasePowerOff] = string(claim.UID)
	return r.Update(ctx, host)
}

// releaseOp builds the release of host: the exit operation (exitOp) of the
// node that the host, or the claim's hand-off record, names. When that
// TalosNode is gone, the release only shuts the machine down. cause says why
// the claim gives the machine back.
//
// The eject list is fixed when the operation is created, so it is built only
// from slots that were read. While no reachable, inventoried endpoint fronts
// the host, releaseOp returns nil and says what it waits for; the endpoint
// and host watches bring the claim back when that changes. The power-off
// could not run before then either.
func (r *HostClaimReconciler) releaseOp(ctx context.Context, claim *metalv1alpha1.HostClaim, host *metalv1alpha1.Host, name, cause string,
	provisionOps []metalv1alpha1.HostOperation) (*metalv1alpha1.HostOperation, string, string, error) {
	shape, err := r.endpointShape(ctx, host)
	if err != nil {
		return nil, "", "", err
	}
	if !shape.known {
		return nil, metalv1alpha1.ReasonReleasing, fmt.Sprintf("releasing host %s waits until a reachable management path has read its media slots, "+
			"so that the release can eject them; see: kubectl get host %s -o wide", host.Name, host.Name), nil
	}
	var nodeName string
	switch {
	case host.Spec.OS != nil:
		nodeName = host.Spec.OS.NodeRef.Name
	case claim.Status.HandOff != nil && claim.Status.HandOff.Host == host.Name:
		nodeName = claim.Status.HandOff.Node
	}
	var node *osv1alpha1.TalosNode
	if nodeName != "" {
		node = &osv1alpha1.TalosNode{}
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, node); apierrors.IsNotFound(err) {
			node = nil
		} else if err != nil {
			return nil, "", "", err
		}
	}
	// The claim's deletion or new machine asked for this release.
	op, reason, msg, err := r.exitOp(ctx, claim, host, node, nodeName, name, purposeRelease,
		metalv1alpha1.RequestedBy(claim.Annotations), shape.insertable, provisionOps)
	if reason != "" {
		msg = "the claim " + cause + ", but " + msg
	}
	return op, reason, msg, err
}

// exitOp builds the operation that takes node out of its cluster, shuts
// host down and ejects each slot in insertable (see removal.go for the
// steps). node is nil when the TalosNode is gone; nodeName still names it.
// When there is no node, the node does not answer, or the executor cannot
// run its steps, the operation only shuts the machine down: the OS first,
// forced off after the release shutdown timeout. It then carries
// AnnotationDiskNotWiped, and the caller marks the host with it before it
// creates the operation. exitOp returns nil, with the reason, while a
// control plane may not leave etcd yet (etcdExitGuard).
func (r *HostClaimReconciler) exitOp(ctx context.Context, claim *metalv1alpha1.HostClaim, host *metalv1alpha1.Host,
	node *osv1alpha1.TalosNode, nodeName, name, purpose string, requestedBy, insertable []string,
	provisionOps []metalv1alpha1.HostOperation) (*metalv1alpha1.HostOperation, string, string, error) {
	var steps []metalv1alpha1.HostOperationAction
	wipes, cluster := false, "unknown-cluster"
	if node != nil {
		member, last, reason, msg, err := r.etcdExitGuard(ctx, claim, node, name)
		if err != nil || reason != "" {
			return nil, reason, msg, err
		}
		steps, wipes, _ = r.exitSteps(node, member, last, "")
		cluster = node.Spec.ClusterRef.Name
	}
	graceful := int32(r.releaseShutdownTimeout().Seconds())
	if wipes {
		// The reset ends with the machine off; the power-off only proves it.
		graceful = 0
	}
	actions := append(steps, metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{GracefulTimeoutSeconds: graceful}})
	for _, s := range insertable {
		actions = append(actions, metalv1alpha1.HostOperationAction{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: s}})
	}
	labels := claimLabels(claim, purpose)
	if nodeName != "" {
		labels[labelNode] = nodeName
	}
	var annotations map[string]string
	if !wipes {
		annotations = map[string]string{metalv1alpha1.AnnotationDiskNotWiped: cluster + "/" + nodeName}
	}
	return &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.OperationsNamespace, Labels: labels, Annotations: annotations},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:                 metalv1alpha1.Reference{Name: host.Name},
			ProposedBy:              r.Proposer,
			RequestedBy:             requestedBy,
			Reason:                  reasonRelease,
			Actions:                 actions,
			Approval:                metalv1alpha1.OperationApproval{Required: ptr.To(releaseNeedsPerson(claim, host, provisionOps))},
			TTLSecondsAfterFinished: ptr.To(releaseTTL),
		},
	}, "", "", nil
}

// describeRelease says in a clause what an approved release does.
func describeRelease(op *metalv1alpha1.HostOperation) string {
	var parts []string
	for _, a := range op.Spec.Actions {
		switch {
		case a.Drain != nil:
			parts = append(parts, "drains node "+a.Drain.NodeRef.Name)
		case a.EtcdLeave != nil:
			parts = append(parts, "takes it out of etcd")
		case a.EtcdRemoveMember != nil:
			parts = append(parts, fmt.Sprintf("removes its etcd member %s through a peer", a.EtcdRemoveMember.MemberID))
		case a.TalosReset != nil:
			parts = append(parts, "wipes the machine through Talos")
		case a.DeleteKubernetesNode != nil:
			parts = append(parts, "deletes its Kubernetes Node")
		case a.PowerOff != nil && a.PowerOff.GracefulTimeoutSeconds > 0:
			parts = append(parts, fmt.Sprintf("shuts the machine down (forced off after %s)", gracefulWindow(a.PowerOff)))
		case a.PowerOff != nil:
			parts = append(parts, "powers it off")
		}
	}
	parts = append(parts, "ejects its media")
	s := strings.Join(parts, ", ")
	if !wipesDisk(op) {
		s += "; its disk is not wiped, so the host is marked for a wipe (pk-metal wipe " + op.Spec.HostRef.Name + ") before it is used again"
	}
	return s
}

// releaseNeedsPerson: the exit operation of host waits for a person. It does
// when the machine runs its node now or ran it on record, unless the claim
// waived approval (provisioning.approval: Automatic). A machine that runs its
// node is a server, and to turn it off is as disruptive as to install it.
// The records count, not only its readiness now (installedOnRecord):
// readiness drops for a Talos upgrade, a kubelet restart or one failed dial,
// and a release made in such a moment would power off a production node with
// nobody asked. A machine that never got that far (no hand-off, no boot that
// reached its check-in) is released pre-cleared: the deletion of the claim
// was the decision, and admission lets only the operator create such an
// operation.
//
// The gate is decided once, when the operation is made. A node that comes up
// after a pre-cleared release was created does not make that release wait
// for a person: approval.required is frozen.
func releaseNeedsPerson(claim *metalv1alpha1.HostClaim, host *metalv1alpha1.Host, provisionOps []metalv1alpha1.HostOperation) bool {
	return !approvalWaived(claim) && (runsNode(host) || installedOnRecord(claim, host.Name, provisionOps))
}

// runsNode: the host reports a running, ready OS.
func runsNode(host *metalv1alpha1.Host) bool {
	return meta.IsStatusConditionTrue(host.Status.Conditions, metalv1alpha1.ConditionOSReady)
}

// installedOnRecord: a record says the host ran the claim's node, whether or
// not it answers now. Either the claim's hand-off names the host (an install
// of ours, or a node found already running), or a boot of the claim on the
// host recorded its check-in step: the node ran its config, and only the
// hand-off was not written yet.
func installedOnRecord(claim *metalv1alpha1.HostClaim, hostName string, provisionOps []metalv1alpha1.HostOperation) bool {
	if h := claim.Status.HandOff; h != nil && h.Host == hostName {
		return true
	}
	return slices.ContainsFunc(provisionOps, func(op metalv1alpha1.HostOperation) bool {
		return op.Spec.HostRef.Name == hostName && checkInRecorded(&op)
	})
}

// checkInRecorded: the operation recorded an awaitCheckIn step as succeeded.
func checkInRecorded(op *metalv1alpha1.HostOperation) bool {
	n := recordedSteps(op)
	for i := range n {
		if op.Spec.Actions[i].AwaitCheckIn != nil {
			return true
		}
	}
	return false
}

// approvalWaived: the claim's provisioning approval is Automatic.
func approvalWaived(claim *metalv1alpha1.HostClaim) bool {
	return claim.Spec.Provisioning != nil && claim.Spec.Provisioning.Approval == metalv1alpha1.ProvisioningApprovalAutomatic
}

// releaseShutdownTimeout is how long a release lets the OS shut down before
// the machine is forced off.
func (r *HostClaimReconciler) releaseShutdownTimeout() time.Duration {
	if r.ReleaseShutdownTimeout > 0 {
		return r.ReleaseShutdownTimeout
	}
	return defaultReleaseShutdownTimeout
}

func short8(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// claimsForOperation maps an operation back to the claim it serves.
func claimsForOperation(_ context.Context, obj client.Object) []ctrl.Request {
	l := obj.GetLabels()
	if l[labelClaimName] == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: l[labelClaimNamespace], Name: l[labelClaimName]}}}
}
