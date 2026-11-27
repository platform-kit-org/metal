package pivot

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
)

// Keys the operator puts on objects that carry a HostClaim's UID
// (internal/controller/provisioning.go). A claim has another UID on the
// target, so each is rewritten.
const (
	labelClaimUID             = "metal.platform-kit.org/claim-uid"
	annotationReleasePowerOff = "metal.platform-kit.org/release-power-off"
	// impersonatedMover is the user the move creates HostClaims as: the
	// claim policy lets only an admin name a TalosNode.
	impersonatedMover = "pk-metal-pivot"
	// settingsConfigMap holds the site settings that the admission policies
	// read (config/policy/hostoperation-approval.yaml).
	settingsConfigMap = "hostoperation-approval"
)

// movedKind is one kind of the operator's state, in the order it is copied:
// what an object refers to is copied before it, where the references allow.
type movedKind struct {
	gvk schema.GroupVersionKind
	// core: a client-go kind; only objects in the operator namespace move.
	core bool
	// namespaced: the objects live in namespaces (all of them move).
	namespaced bool
	// skip leaves an object of this kind behind.
	skip func(*unstructured.Unstructured) bool
}

var (
	metalGV = metalv1alpha1.GroupVersion
	osGV    = osv1alpha1.GroupVersion
)

// movedKinds is the copy order. Secrets come early: an endpoint's rotated
// credentials must be in place before anything could rotate them again. A
// Secret owned by an object copied later gets its owner reference in the
// second pass, before the new manager starts.
var movedKinds = []movedKind{
	{gvk: corev1.SchemeGroupVersion.WithKind("ConfigMap"), core: true, namespaced: true, skip: func(u *unstructured.Unstructured) bool {
		return u.GetName() == "kube-root-ca.crt"
	}},
	{gvk: corev1.SchemeGroupVersion.WithKind("Secret"), core: true, namespaced: true, skip: func(u *unstructured.Unstructured) bool {
		t, _, _ := unstructured.NestedString(u.Object, "type")
		return t == string(corev1.SecretTypeServiceAccountToken) || t == string(corev1.SecretTypeBootstrapToken)
	}},
	{gvk: metalGV.WithKind("Asset")},
	{gvk: metalGV.WithKind("ManagementEndpoint")},
	{gvk: metalGV.WithKind("EndpointProfile")},
	{gvk: metalGV.WithKind("BiosProfile")},
	{gvk: metalGV.WithKind("FirmwareProfile")},
	{gvk: metalGV.WithKind("BiosSchema")},
	{gvk: metalGV.WithKind("BiosSettings")},
	{gvk: metalGV.WithKind("Host")},
	{gvk: metalGV.WithKind("HostClaim"), namespaced: true},
	{gvk: metalGV.WithKind("HostOperation"), namespaced: true},
	{gvk: osGV.WithKind("TalosCluster")},
	{gvk: osGV.WithKind("TalosNode")},
	{gvk: osGV.WithKind("BootAsset")},
	{gvk: metalGV.WithKind("MaintenanceWindow")},
}

// movedKindOf reports whether an owner reference names a kind the move
// copies.
func movedKindOf(apiVersion, kind string) bool {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return false
	}
	return slices.Contains(movedGroups, gv.Group) ||
		slices.ContainsFunc(movedKinds, func(k movedKind) bool { return k.gvk == gv.WithKind(kind) })
}

// Report is what a copy did, and what the verification compares against.
type Report struct {
	// Copied counts the objects per kind.
	Copied map[string]int
	// UIDs maps every copied object's source UID to its target UID.
	UIDs map[types.UID]types.UID
	// Rewritten counts the UID references rewritten to target UIDs.
	Rewritten int
	// Operations are the HostOperations (namespace/name) the copy left on
	// the target: anything else there was proposed after the move.
	Operations map[string]bool
	// HandedOff are the claims (namespace/name) whose hand-off record was
	// copied: each must settle as handed off, never install again.
	HandedOff []string
	// Pruned lists the objects removed from the target because the source
	// no longer has them.
	Pruned []string
}

// sourceObject is one object read from the source, with its kind.
type sourceObject struct {
	kind movedKind
	obj  *unstructured.Unstructured
}

// rewriter maps source UIDs to target UIDs in an object about to be written.
type rewriter struct {
	uids map[types.UID]types.UID
	// claims are the UIDs of the source's HostClaims: a reference to one of
	// them must be rewritten, a reference to any other is a record of a claim
	// that is gone on both sides and stays as it is.
	claims map[types.UID]bool
}

// rewrite returns the object as it is to be written to the target: server
// fields dropped, owner references and claim UIDs rewritten. complete is
// false when a reference could not be rewritten yet (its object is copied
// later); such a reference is left out, never written with a UID the target
// does not know, which the garbage collector would act on.
func (r *rewriter) rewrite(src *unstructured.Unstructured) (out *unstructured.Unstructured, rewritten int, complete bool) {
	out = sanitize(src)
	complete = true

	var owners []metav1.OwnerReference
	for _, ref := range src.GetOwnerReferences() {
		if uid, ok := r.uids[ref.UID]; ok {
			ref.UID = uid
			owners = append(owners, ref)
			rewritten++
			continue
		}
		if movedKindOf(ref.APIVersion, ref.Kind) {
			complete = false
		}
		// An owner the move does not carry (a cert-manager request) is
		// dropped: on the target it does not exist.
	}
	out.SetOwnerReferences(owners)

	claimUID := func(v string) (string, bool) {
		if v == "" || !r.claims[types.UID(v)] {
			return v, true
		}
		if uid, ok := r.uids[types.UID(v)]; ok {
			rewritten++
			return string(uid), true
		}
		return v, false
	}
	if labels := out.GetLabels(); labels[labelClaimUID] != "" {
		v, ok := claimUID(labels[labelClaimUID])
		labels[labelClaimUID] = v
		out.SetLabels(labels)
		complete = complete && ok
	}
	if ann := out.GetAnnotations(); ann[annotationReleasePowerOff] != "" {
		v, ok := claimUID(ann[annotationReleasePowerOff])
		ann[annotationReleasePowerOff] = v
		out.SetAnnotations(ann)
		complete = complete && ok
	}
	if v, found, _ := unstructured.NestedString(out.Object, "spec", "claimRef", "uid"); found {
		nv, ok := claimUID(v)
		_ = unstructured.SetNestedField(out.Object, nv, "spec", "claimRef", "uid")
		complete = complete && ok
	}
	return out, rewritten, complete
}

// sanitize drops what the source's API server assigned.
func sanitize(src *unstructured.Unstructured) *unstructured.Unstructured {
	out := src.DeepCopy()
	for _, f := range []string{"uid", "resourceVersion", "creationTimestamp", "managedFields", "generation",
		"deletionTimestamp", "deletionGracePeriodSeconds", "selfLink"} {
		unstructured.RemoveNestedField(out.Object, "metadata", f)
	}
	return out
}

// Copy copies the operator's state from the source to the target: first
// every object with its metadata and spec, in dependency order; then once
// more, now that every UID is known, to fill in the references the first
// pass could not; then every status. It is safe to run again: an object
// already on the target is brought up to the source's version.
func (m *Mover) Copy(ctx context.Context, plan *Plan) (*Report, error) {
	rep := &Report{Copied: map[string]int{}, UIDs: map[types.UID]types.UID{}, Operations: map[string]bool{}}
	objs, err := m.readSource(ctx)
	if err != nil {
		return nil, err
	}
	rw := &rewriter{uids: rep.UIDs, claims: map[types.UID]bool{}}
	for _, so := range objs {
		if so.kind.gvk.Kind == "HostClaim" {
			rw.claims[so.obj.GetUID()] = true
		}
	}
	if err := m.ensureNamespaces(ctx, objs); err != nil {
		return nil, err
	}
	if plan != nil && plan.Prune {
		if rep.Pruned, err = m.prune(ctx, objs); err != nil {
			return nil, err
		}
	}

	for pass := 1; pass <= 2; pass++ {
		rep.Rewritten = 0
		for _, so := range objs {
			n, err := m.ensure(ctx, so, rw)
			if err != nil {
				return nil, fmt.Errorf("copy %s %s: %w", so.kind.gvk.Kind, objectRef(so.obj), err)
			}
			rep.Rewritten += n
		}
	}
	for _, so := range objs {
		if err := m.copyStatus(ctx, so); err != nil {
			return nil, fmt.Errorf("copy the status of %s %s: %w", so.kind.gvk.Kind, objectRef(so.obj), err)
		}
		rep.Copied[so.kind.gvk.Kind]++
		switch so.kind.gvk.Kind {
		case "HostOperation":
			rep.Operations[objectRef(so.obj)] = true
		case "HostClaim":
			if h, found, _ := unstructured.NestedMap(so.obj.Object, "status", "handOff"); found && len(h) > 0 {
				rep.HandedOff = append(rep.HandedOff, objectRef(so.obj))
			}
		}
	}
	m.logf("copied %s; %d UID references rewritten", copiedSummary(m.movedKinds(), rep.Copied), rep.Rewritten)
	return rep, nil
}

func copiedSummary(kinds []movedKind, copied map[string]int) string {
	var parts []string
	for _, k := range kinds {
		if n := copied[k.gvk.Kind]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k.gvk.Kind))
		}
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return fmt.Sprint(parts)
}

// readSource lists every object that moves, kind by kind in copy order and
// by name within a kind, so that two runs copy in the same order.
func (m *Mover) readSource(ctx context.Context) ([]sourceObject, error) {
	var out []sourceObject
	for _, k := range m.movedKinds() {
		var items []unstructured.Unstructured
		namespaces := []string{""} // all of them
		if k.core {
			namespaces = m.coreNamespaces()
		}
		for _, ns := range namespaces {
			got, err := listServed(ctx, m.Source, k.gvk, client.InNamespace(ns))
			if err != nil {
				return nil, err
			}
			items = append(items, got...)
		}
		sort.Slice(items, func(i, j int) bool { return objectRef(&items[i]) < objectRef(&items[j]) })
		for i := range items {
			if k.skip == nil || !k.skip(&items[i]) {
				out = append(out, sourceObject{kind: k, obj: &items[i]})
			}
		}
	}
	return out, nil
}

// coreNamespaces are the namespaces whose ConfigMaps and Secrets move.
func (m *Mover) coreNamespaces() []string {
	if len(m.coreNS) == 0 {
		return []string{m.Options.Namespace}
	}
	return m.coreNS
}

// ensureNamespaces creates the namespaces the claims and the credentials
// live in, with the source's labels.
func (m *Mover) ensureNamespaces(ctx context.Context, objs []sourceObject) error {
	seen := map[string]bool{m.Options.Namespace: true}
	for _, so := range objs {
		ns := so.obj.GetNamespace()
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		src := &corev1.Namespace{}
		if err := m.Source.Client.Get(ctx, client.ObjectKey{Name: ns}, src); err != nil {
			return err
		}
		dst := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: src.Labels}}
		if err := m.Target.Client.Create(ctx, dst); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create namespace %s on %s: %w", ns, m.Target.Name, err)
		}
	}
	return nil
}

// kindNotYetKnown is the API server's answer to a write of a kind whose CRD
// it established a moment ago. A mutating admission policy (the name
// stamps) checks the object against the OpenAPI schema of its kind, and the
// API server publishes that schema after the CRD is Established.
const kindNotYetKnown = "There can be a delay between when CustomResourceDefinitions are created and when they are available"

// untilAdmitted runs write, and again while the admission of the target does
// not know the kind yet. The first write of each kind after Install can meet
// this; any other error ends it.
func (m *Mover) untilAdmitted(ctx context.Context, write func() error) error {
	return m.wait(ctx, "the admission policies of "+m.Target.Name+" to know the operator's kinds",
		func(context.Context) (bool, error) {
			err := write()
			if err != nil && strings.Contains(err.Error(), kindNotYetKnown) {
				return false, nil
			}
			return true, err
		})
}

// approvalFields are the fields of spec.approval that only the approver
// sets, for each kind with an approval gate
// (config/policy/hostoperation-approval.yaml, talosrollout-approval.yaml).
var approvalFields = map[string][]string{
	"HostOperation": {"approvedBy"},
	"TalosRollout":  {"approvedBy", "planHash"},
}

// writerFor is the client an object is created and updated with, as an
// admin (adminGroups). An operation or a rollout is written as the user who
// proposed it: the approval policies check spec.proposedBy against the
// request's user. An object whose change makes the operator propose work is
// written as the user its requested-by annotation names (stamped first, see
// stampRequester): the requested-by policy wants the request's user there.
// Any other object is written as the move: the guards of the site settings,
// the profiles and the backup policies let only an admin write them, and the
// claim policy lets only an admin name a TalosNode.
func (m *Mover) writerFor(ctx context.Context, so sourceObject, want *unstructured.Unstructured) (client.Client, error) {
	user := impersonatedMover
	if _, gated := approvalFields[so.kind.gvk.Kind]; gated {
		user, _, _ = unstructured.NestedString(want.Object, "spec", "proposedBy")
	} else if by := want.GetAnnotations()[metalv1alpha1.AnnotationRequestedBy]; by != "" && requestGated(want) {
		user = by
	}
	return m.Target.as(user, m.adminGroups(ctx)...)
}

// requestGated: the requested-by policies (config/policy/requested-by.yaml,
// requested-by-patches.yaml) ask for the requester's name when obj is
// created, changed or deleted, where the site forbids self-approval. The
// move names one in both modes, for the audit trail.
// A Host only while it asks for a wipe; a ConfigMap only when it is a Talos
// config patch.
func requestGated(obj *unstructured.Unstructured) bool {
	switch obj.GetKind() {
	case "TalosCluster", "TalosNode", "HostClaim", "BiosProfile", "FirmwareProfile":
		return true
	case "Host":
		_, ok := obj.GetAnnotations()[metalv1alpha1.AnnotationWipe]
		return ok
	case "ConfigMap":
		_, ok := obj.GetLabels()[osv1alpha1.LabelPatch]
		return ok
	}
	return false
}

// stampRequester names the move as the requester of a gated object that
// names nobody (written before the policy asked for a name). An object
// that names a person keeps the name, and is written as that person.
func stampRequester(obj *unstructured.Unstructured) {
	if !requestGated(obj) || obj.GetAnnotations()[metalv1alpha1.AnnotationRequestedBy] != "" {
		return
	}
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[metalv1alpha1.AnnotationRequestedBy] = impersonatedMover
	obj.SetAnnotations(ann)
}

// adminGroups are the groups an impersonated request carries: the
// operator's admin group (from the policies' parameters on the target) and
// system:masters, so that RBAC lets the replayed user write.
func (m *Mover) adminGroups(ctx context.Context) []string {
	groups := []string{"system:masters", "system:authenticated"}
	params := &corev1.ConfigMap{}
	if err := m.Target.Client.Get(ctx, client.ObjectKey{Namespace: m.Options.Namespace, Name: settingsConfigMap}, params); err == nil {
		if g := params.Data["adminGroup"]; g != "" {
			groups = append(groups, g)
		}
	}
	return groups
}

// ensure creates the object on the target, or brings its metadata and spec
// up to the source's. It records the object's target UID and returns how
// many references it rewrote.
func (m *Mover) ensure(ctx context.Context, so sourceObject, rw *rewriter) (int, error) {
	want, n, complete := rw.rewrite(so.obj)
	unstructured.RemoveNestedField(want.Object, "status")
	stampRequester(want)
	lifts := takeLifts(want)
	// Admission refuses an operation or a rollout created approved; the
	// approval is replayed below, by the approver.
	fields := approvalFields[so.kind.gvk.Kind]
	approval := map[string]any{}
	for _, f := range fields {
		if v, _, _ := unstructured.NestedString(want.Object, "spec", "approval", f); v != "" {
			approval[f] = v
		}
		unstructured.RemoveNestedField(want.Object, "spec", "approval", f)
	}

	cur := &unstructured.Unstructured{}
	cur.SetGroupVersionKind(so.kind.gvk)
	err := m.Target.Client.Get(ctx, client.ObjectKeyFromObject(want), cur)
	switch {
	case apierrors.IsNotFound(err):
		w, err := m.writerFor(ctx, so, want)
		if err != nil {
			return 0, err
		}
		if err := m.untilAdmitted(ctx, func() error { return w.Create(ctx, want) }); err != nil {
			return 0, err
		}
		cur = want
	case err != nil:
		return 0, err
	case !complete:
		// The object is there already, and a reference in it cannot be
		// rewritten yet: the next pass updates it, and an update now would
		// write the source's UID over the target's.
	default:
		keepLifts(cur, want, lifts)
		if mergeInto(cur, want, fields) {
			w, err := m.writerFor(ctx, so, want)
			if err != nil {
				return 0, err
			}
			if err := m.untilAdmitted(ctx, func() error { return w.Update(ctx, cur) }); err != nil {
				return 0, err
			}
		}
	}
	rw.uids[so.obj.GetUID()] = cur.GetUID()
	if err := m.replayLifts(ctx, so, cur, lifts); err != nil {
		return 0, err
	}
	if err := m.replayApproval(ctx, cur, approval); err != nil {
		return 0, err
	}
	return n, nil
}

// replayApproval sets the source's approval on cur again, as the approver:
// the approval policies check that approvedBy names the request's user.
func (m *Mover) replayApproval(ctx context.Context, cur *unstructured.Unstructured, approval map[string]any) error {
	approvedBy, _ := approval["approvedBy"].(string)
	got, _, _ := unstructured.NestedString(cur.Object, "spec", "approval", "approvedBy")
	if approvedBy == "" || got == approvedBy {
		return nil
	}
	if got != "" {
		return fmt.Errorf("approved by %s on %s but by %s on %s; an approval cannot change", approvedBy, m.Source.Name, got, m.Target.Name)
	}
	as, err := m.Target.as(approvedBy, m.adminGroups(ctx)...)
	if err != nil {
		return err
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"approval": approval}})
	if err := as.Patch(ctx, cur, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("replay the approval by %s: %w", approvedBy, err)
	}
	proposedBy, _, _ := unstructured.NestedString(cur.Object, "spec", "proposedBy")
	requestedBy, _, _ := unstructured.NestedStringSlice(cur.Object, "spec", "requestedBy")
	planHash, _ := approval["planHash"].(string)
	m.Options.Audit.ApprovalReplayed(auditObject(cur), proposedBy, requestedBy, approvedBy, planHash)
	return nil
}

// mergeInto brings cur's labels, annotations, finalizers, owner references
// and content up to want's and reports whether anything changed. The
// approval fields keep cur's values: ensure replays the source's.
func mergeInto(cur, want *unstructured.Unstructured, approval []string) bool {
	changed := false
	set := func(eq bool, apply func()) {
		if !eq {
			apply()
			changed = true
		}
	}
	set(maps.Equal(cur.GetLabels(), want.GetLabels()), func() { cur.SetLabels(want.GetLabels()) })
	set(maps.Equal(cur.GetAnnotations(), want.GetAnnotations()), func() { cur.SetAnnotations(want.GetAnnotations()) })
	set(slices.Equal(cur.GetFinalizers(), want.GetFinalizers()), func() { cur.SetFinalizers(want.GetFinalizers()) })
	set(equality.Semantic.DeepEqual(cur.GetOwnerReferences(), want.GetOwnerReferences()),
		func() { cur.SetOwnerReferences(want.GetOwnerReferences()) })
	for k, v := range want.Object {
		if k == "metadata" || k == "status" || k == "apiVersion" || k == "kind" {
			continue
		}
		if k == "spec" && len(approval) > 0 {
			spec := &unstructured.Unstructured{Object: map[string]any{"spec": runtime.DeepCopyJSONValue(v)}}
			for _, f := range approval {
				if got, found, _ := unstructured.NestedString(cur.Object, "spec", "approval", f); found {
					_ = unstructured.SetNestedField(spec.Object, got, "spec", "approval", f)
				} else {
					unstructured.RemoveNestedField(spec.Object, "spec", "approval", f)
				}
			}
			v = spec.Object["spec"]
		}
		set(equality.Semantic.DeepEqual(cur.Object[k], v), func() { cur.Object[k] = v })
	}
	return changed
}

// copyStatus writes the source's status onto the target object. The Host's
// and the endpoint's status have several writers, each with its own field
// manager; they are applied as those writers apply them.
func (m *Mover) copyStatus(ctx context.Context, so sourceObject) error {
	status, found, _ := unstructured.NestedFieldNoCopy(so.obj.Object, "status")
	if !found || status == nil {
		return nil
	}
	cur := &unstructured.Unstructured{}
	cur.SetGroupVersionKind(so.kind.gvk)
	if err := m.Target.Client.Get(ctx, client.ObjectKeyFromObject(so.obj), cur); err != nil {
		return err
	}
	if !so.kind.core {
		typed, err := typedObject(so.obj)
		if err != nil {
			return err
		}
		if handled, err := controller.ApplyStatusShares(ctx, m.Target.Client, typed); handled || err != nil {
			return err
		}
	}
	if equality.Semantic.DeepEqual(cur.Object["status"], status) {
		return nil
	}
	cur.Object["status"] = runtime.DeepCopyJSONValue(status)
	err := m.Target.Client.Status().Update(ctx, cur)
	if apierrors.IsNotFound(err) {
		// A kind without a status subresource took its status with the
		// object itself.
		return nil
	}
	return err
}

var typedScheme = func() *runtime.Scheme {
	s, err := pkscheme.New()
	if err != nil {
		panic(err) // static registration
	}
	return s
}()

// typedObject converts an operator object to its Go type.
func typedObject(u *unstructured.Unstructured) (client.Object, error) {
	o, err := typedScheme.New(u.GroupVersionKind())
	if err != nil {
		return nil, err
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, o); err != nil {
		return nil, err
	}
	co, ok := o.(client.Object)
	if !ok {
		return nil, fmt.Errorf("%s is not an object", u.GroupVersionKind())
	}
	return co, nil
}

// prune removes from the target the operator's objects the source does not
// have: what an older copy left behind (deleted on the other side since).
// Its finalizers come off first, because no manager runs on the target to
// take them off.
func (m *Mover) prune(ctx context.Context, objs []sourceObject) ([]string, error) {
	keep := map[string]bool{}
	for _, so := range objs {
		keep[so.kind.gvk.Kind+" "+objectRef(so.obj)] = true
	}
	var pruned []string
	kinds := m.movedKinds()
	for i := len(kinds) - 1; i >= 0; i-- {
		k := kinds[i]
		if k.core {
			continue
		}
		items, err := listServed(ctx, m.Target, k.gvk)
		if err != nil {
			return nil, err
		}
		for j := range items {
			u := &items[j]
			key := k.gvk.Kind + " " + objectRef(u)
			if keep[key] {
				continue
			}
			// A delete of a gated object must name its requester first: the
			// move, which takes the delete on.
			w := m.Target.Client
			stamped := false
			if requestGated(u) {
				mover, err := m.Target.as(impersonatedMover, m.adminGroups(ctx)...)
				if err != nil {
					return nil, err
				}
				w = mover
				stamped = u.GetAnnotations()[metalv1alpha1.AnnotationRequestedBy] != impersonatedMover
				ann := u.GetAnnotations()
				if ann == nil {
					ann = map[string]string{}
				}
				ann[metalv1alpha1.AnnotationRequestedBy] = impersonatedMover
				u.SetAnnotations(ann)
			}
			if len(u.GetFinalizers()) > 0 || stamped {
				u.SetFinalizers(nil)
				if err := w.Update(ctx, u); client.IgnoreNotFound(err) != nil {
					return nil, err
				}
			}
			if err := w.Delete(ctx, u); client.IgnoreNotFound(err) != nil {
				return nil, err
			}
			m.Options.Audit.Deleted(auditObject(u), audit.DeletePivotPruned,
				fmt.Sprintf("%s no longer has it; an earlier move left it on %s", m.Source.Name, m.Target.Name))
			pruned = append(pruned, key)
			m.logf("removed %s from %s: %s no longer has it", key, m.Target.Name, m.Source.Name)
		}
	}
	return pruned, nil
}

// auditObject names obj in the audit trail, with its UID on the target.
func auditObject(obj client.Object) audit.Object {
	return audit.Object{Kind: audit.Kind(obj.GetObjectKind().GroupVersionKind().Kind), Namespace: obj.GetNamespace(),
		Name: obj.GetName(), UID: obj.GetUID()}
}
