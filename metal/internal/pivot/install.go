package pivot

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// operatorGroups are the API groups of the operator's own kinds.
var operatorGroups = []string{metalv1alpha1.GroupVersion.Group, osv1alpha1.GroupVersion.Group}

// copyStep is one kind that Install copies from the source as it is there.
type copyStep struct {
	gvk schema.GroupVersionKind
	// namespaced: only the objects in the operator namespace move.
	namespaced bool
	// keep selects the objects that move; nil moves all of them.
	keep func(*unstructured.Unstructured) bool
	// drop names the fields that belong to the source cluster.
	drop [][]string
}

// operatorNamed: the cluster-wide RBAC names the operator, so it carries
// its prefix.
func operatorNamed(u *unstructured.Unstructured) bool {
	return strings.HasPrefix(u.GetName(), "pk-metal")
}

// installSteps are the kinds that Install copies after the namespaces: the
// cluster-wide RBAC (the manager's roles, the approver role and their
// bindings), then in the operator namespace the service accounts, the roles,
// the Services and the NetworkPolicies.
var installSteps = []copyStep{
	{gvk: rbacv1.SchemeGroupVersion.WithKind("ClusterRole"), keep: operatorNamed},
	{gvk: rbacv1.SchemeGroupVersion.WithKind("ClusterRoleBinding"), keep: operatorNamed},
	// A token Secret of a service account is the source's.
	{gvk: corev1.SchemeGroupVersion.WithKind("ServiceAccount"), namespaced: true, drop: [][]string{{"secrets"}},
		keep: func(u *unstructured.Unstructured) bool { return u.GetName() != "default" }},
	{gvk: rbacv1.SchemeGroupVersion.WithKind("Role"), namespaced: true},
	{gvk: rbacv1.SchemeGroupVersion.WithKind("RoleBinding"), namespaced: true},
	// Addresses belong to the source's service network.
	{gvk: corev1.SchemeGroupVersion.WithKind("Service"), namespaced: true,
		drop: [][]string{{"spec", "clusterIP"}, {"spec", "clusterIPs"}, {"spec", "ipFamilies"}, {"spec", "ipFamilyPolicy"}}},
	// The media port answers the BMC network only, on the target as on the
	// source.
	{gvk: networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"), namespaced: true},
}

// Install puts the operator onto the target, from what runs on the source:
// the same manifests the source was installed with, read back from its API
// server. The order is the one a fresh install needs: the CRDs (and the wait
// until they are served), the admission policies, the namespaces and the
// policies' parameters, the installSteps, the claims the manager mounts
// (empty: their contents do not move) and the manager's Deployment at zero
// replicas. Every object is applied server-side, so a rerun changes only
// what differs.
func (m *Mover) Install(ctx context.Context) error {
	o := m.Options
	src := m.Source.Client
	if err := m.installCRDs(ctx); err != nil {
		return err
	}

	// The admission policies guard the operator's kinds; their bindings name
	// their parameters, which live in a namespace.
	params := map[types.NamespacedName]bool{}
	policies := 0
	for _, kind := range []string{"ValidatingAdmissionPolicy", "MutatingAdmissionPolicy"} {
		n, err := m.installPolicies(ctx, kind, params)
		if err != nil {
			return err
		}
		policies += n
	}
	m.logf("installed %d admission policies", policies)

	if err := m.installNamespaces(ctx, params); err != nil {
		return err
	}
	for _, s := range installSteps {
		if _, err := m.copyKind(ctx, s); err != nil {
			return err
		}
	}

	claims, err := m.claimMoves(ctx)
	if err != nil {
		return err
	}
	for _, c := range claims {
		if c.Action != ClaimCreate {
			continue
		}
		if err := m.applyTyped(ctx, targetClaim(c)); err != nil {
			return err
		}
		m.logf("made PersistentVolumeClaim %s/%s, empty", o.Namespace, c.Name)
	}
	d := &appsv1.Deployment{}
	if err := src.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Deployment}, d); err != nil {
		return fmt.Errorf("read Deployment %s/%s on %s: %w", o.Namespace, o.Deployment, m.Source.Name, err)
	}
	if err := m.applyTyped(ctx, managerForTarget(d, o.Image, o.MediaURL, claims)); err != nil {
		return err
	}
	m.logf("installed the operator in namespace %s (Deployment %s at 0 replicas)", o.Namespace, o.Deployment)
	return nil
}

// installCRDs copies the CRDs of the operator's groups, and waits until the
// target serves them.
func (m *Mover) installCRDs(ctx context.Context) error {
	crds, err := m.copyKind(ctx, copyStep{gvk: crdGVK, keep: func(u *unstructured.Unstructured) bool {
		group, _, _ := unstructured.NestedString(u.Object, "spec", "group")
		return slices.Contains(operatorGroups, group)
	}})
	if err != nil {
		return err
	}
	if len(crds) == 0 {
		return fmt.Errorf("%s serves none of the operator's CRDs", m.Source.Name)
	}
	if err := m.wait(ctx, "the CRDs to be served on "+m.Target.Name, func(ctx context.Context) (bool, error) {
		for i := range crds {
			crd := &unstructured.Unstructured{}
			crd.SetGroupVersionKind(crdGVK)
			if err := m.Target.Client.Get(ctx, client.ObjectKey{Name: crds[i].GetName()}, crd); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			if !crdEstablished(crd) {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return err
	}
	m.logf("installed %d CRDs", len(crds))
	return nil
}

// installPolicies copies the admission policies of kind (validating or
// mutating) that guard the operator's objects, and their bindings, and adds
// the parameters the bindings name to params. It returns how many policies
// it copied. A source that does not serve the kind (a mutating one before
// Kubernetes 1.36) has none to copy.
func (m *Mover) installPolicies(ctx context.Context, kind string, params map[types.NamespacedName]bool) (int, error) {
	gv := admissionregistrationv1.SchemeGroupVersion
	policies, err := m.copyKind(ctx, copyStep{gvk: gv.WithKind(kind), keep: func(u *unstructured.Unstructured) bool {
		raw, _, _ := unstructured.NestedMap(u.Object, "spec", "matchConstraints")
		match := &admissionregistrationv1.MatchResources{}
		return runtime.DefaultUnstructuredConverter.FromUnstructured(raw, match) == nil && guardsOperator(match)
	}})
	if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	bindings, err := m.copyKind(ctx, copyStep{gvk: gv.WithKind(kind + "Binding"), keep: func(u *unstructured.Unstructured) bool {
		policy, _, _ := unstructured.NestedString(u.Object, "spec", "policyName")
		return slices.ContainsFunc(policies, func(p unstructured.Unstructured) bool { return p.GetName() == policy })
	}})
	if err != nil {
		return 0, err
	}
	for i := range bindings {
		ns, _, _ := unstructured.NestedString(bindings[i].Object, "spec", "paramRef", "namespace")
		name, _, _ := unstructured.NestedString(bindings[i].Object, "spec", "paramRef", "name")
		if ns != "" && name != "" {
			params[types.NamespacedName{Namespace: ns, Name: name}] = true
		}
	}
	return len(policies), nil
}

// installNamespaces copies the namespaces, the operator's (marked as moved
// here from the source) and the ones the policies' parameters live in, then
// the parameters.
func (m *Mover) installNamespaces(ctx context.Context, params map[types.NamespacedName]bool) error {
	namespaces := []string{m.Options.Namespace}
	for p := range params {
		if !slices.Contains(namespaces, p.Namespace) {
			namespaces = append(namespaces, p.Namespace)
		}
	}
	for _, n := range namespaces {
		ns := &corev1.Namespace{}
		if err := m.Source.Client.Get(ctx, client.ObjectKey{Name: n}, ns); err != nil {
			return fmt.Errorf("read namespace %s on %s: %w", n, m.Source.Name, err)
		}
		ns.Annotations = nil
		if err := m.applyTyped(ctx, ns); err != nil {
			return err
		}
	}
	if err := m.markNamespace(ctx, m.Target, map[string]any{AnnotationSource: m.sourceID, AnnotationMovedTo: nil}); err != nil {
		return err
	}
	for p := range params {
		cm := &corev1.ConfigMap{}
		if err := m.Source.Client.Get(ctx, client.ObjectKey(p), cm); err != nil {
			return fmt.Errorf("read the admission policies' parameters %s on %s: %w", p, m.Source.Name, err)
		}
		if err := m.applyTyped(ctx, cm); err != nil {
			return err
		}
	}
	return nil
}

// copyKind applies to the target each object of one kind on the source that
// s keeps, without the fields s drops, and returns them.
func (m *Mover) copyKind(ctx context.Context, s copyStep) ([]unstructured.Unstructured, error) {
	var opts []client.ListOption
	if s.namespaced {
		opts = append(opts, client.InNamespace(m.Options.Namespace))
	}
	items, err := listKind(ctx, m.Source, s.gvk, opts...)
	if err != nil {
		return nil, err
	}
	var out []unstructured.Unstructured
	for i := range items {
		if s.keep != nil && !s.keep(&items[i]) {
			continue
		}
		for _, f := range s.drop {
			unstructured.RemoveNestedField(items[i].Object, f...)
		}
		if err := m.apply(ctx, &items[i]); err != nil {
			return nil, err
		}
		out = append(out, items[i])
	}
	return out, nil
}

// managerForTarget is the source's Deployment as the target runs it: at zero
// replicas until the state is copied, with the image and media URL the
// target needs. A claim made ephemeral becomes an emptyDir, and the manager
// no longer names it to the imager Jobs: they are other pods and could not
// write into it. A claim copied keeps its name, so the flag that names it
// stays right.
func managerForTarget(src *appsv1.Deployment, image, mediaURL string, claims []ClaimMove) *appsv1.Deployment {
	d := src.DeepCopy()
	zero := int32(0)
	d.Spec.Replicas = &zero
	delete(d.Annotations, "deployment.kubernetes.io/revision")
	var ephemeral []string
	for _, c := range claims {
		if c.Action != ClaimEphemeral {
			continue
		}
		ephemeral = append(ephemeral, c.Name)
		for i := range d.Spec.Template.Spec.Volumes {
			if v := &d.Spec.Template.Spec.Volumes[i]; v.Name == c.Volume {
				v.VolumeSource = ephemeralVolume(c)
			}
		}
	}
	for i := range d.Spec.Template.Spec.Containers {
		c := &d.Spec.Template.Spec.Containers[i]
		if c.Name != "manager" {
			continue
		}
		if image != "" {
			c.Image = image
		}
		if mediaURL != "" {
			c.Args = slices.DeleteFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "--media-url=") })
			c.Args = append(c.Args, "--media-url="+mediaURL)
		}
		c.Args = slices.DeleteFunc(c.Args, func(a string) bool {
			v, ok := strings.CutPrefix(a, storeClaimFlag+"=")
			return ok && slices.Contains(ephemeral, v)
		})
	}
	return d
}

// guardsOperator: the policy matches a resource of the operator's groups,
// the site settings ConfigMap that the other policies read, or the
// ConfigMaps that hold Talos config patches (selected by their label).
func guardsOperator(m *admissionregistrationv1.MatchResources) bool {
	if m == nil {
		return false
	}
	for _, r := range m.ResourceRules {
		for _, g := range r.APIGroups {
			if slices.Contains(operatorGroups, g) {
				return true
			}
		}
		if slices.Contains(r.Resources, "configmaps") &&
			(slices.Contains(r.ResourceNames, settingsConfigMap) || selectsPatches(m.ObjectSelector)) {
			return true
		}
	}
	return false
}

// selectsPatches: the selector selects only objects with the label
// os.platform-kit.org/patch.
func selectsPatches(s *metav1.LabelSelector) bool {
	if s == nil {
		return false
	}
	if _, ok := s.MatchLabels[osv1alpha1.LabelPatch]; ok {
		return true
	}
	for _, e := range s.MatchExpressions {
		if e.Key == osv1alpha1.LabelPatch && (e.Operator == metav1.LabelSelectorOpExists || e.Operator == metav1.LabelSelectorOpIn) {
			return true
		}
	}
	return false
}

func crdEstablished(crd *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
	for _, c := range conds {
		if cm, ok := c.(map[string]any); ok && cm["type"] == "Established" && cm["status"] == "True" {
			return true
		}
	}
	return false
}

// listKind lists the objects of one kind on c.
func listKind(ctx context.Context, c *Cluster, gvk schema.GroupVersionKind, opts ...client.ListOption) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return nil, fmt.Errorf("list %s on %s: %w", gvk.Kind, c.Name, err)
	}
	for i := range list.Items {
		list.Items[i].SetGroupVersionKind(gvk)
	}
	return list.Items, nil
}

// applyTyped applies a client-go object read from the source.
func (m *Mover) applyTyped(ctx context.Context, obj client.Object) error {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	gvk, err := apiutil.GVKForObject(obj, typedScheme)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: raw}
	u.SetGroupVersionKind(gvk)
	return m.apply(ctx, u)
}

// apply writes obj to the target server-side, without what the source's API
// server assigned and without its status.
func (m *Mover) apply(ctx context.Context, obj *unstructured.Unstructured) error {
	u := sanitize(obj)
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "ownerReferences")
	unstructured.RemoveNestedField(u.Object, "metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")
	if err := m.Target.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s %s on %s: %w", u.GetKind(), objectRef(u), m.Target.Name, err)
	}
	return nil
}

// markNamespace sets (a string) or removes (nil) annotations on the
// operator namespace of one side.
func (m *Mover) markNamespace(ctx context.Context, c *Cluster, annotations map[string]any) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	ns := &corev1.Namespace{}
	ns.Name = m.Options.Namespace
	if err := c.Client.Patch(ctx, ns, client.RawPatch(types.MergePatchType, patch)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("mark namespace %s on %s: %w", m.Options.Namespace, c.Name, err)
	}
	return nil
}
