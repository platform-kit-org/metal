package envtest_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// The API server names the requester (config/name-stamp/requested-by-stamp.yaml):
// where the site forbids self-approval, a person who edits with kubectl does
// not set metal.platform-kit.org/requested-by by hand, and the requested-by
// policies still find the name they ask for.
func TestRequestedByStamp(t *testing.T) {
	st := startEnv(t)
	cl := st.cl
	ctx := t.Context()

	installApprovalPolicy(t, ctx, cl, "op-sa")
	installPolicy(t, ctx, cl, "requested-by.yaml")
	installStamp(t, ctx, cl)
	setSelfApproval(t, ctx, cl, ptr.To("Forbidden"))
	alice := st.userClient(t, "alice")
	bob := st.userClient(t, "bob")
	key := metalv1alpha1.AnnotationRequestedBy
	patch := func(c client.Client, obj client.Object, body string) error {
		return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	named := func(obj client.Object) string {
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(obj), obj))
		return obj.GetAnnotations()[key]
	}

	t.Run("an edit with kubectl names its user", func(t *testing.T) {
		cluster := &osv1alpha1.TalosCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "stamp-fleet"},
			Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", KubernetesVersion: "v1.36.2",
				Endpoint:   "https://192.0.2.10:6443",
				SecretsRef: osv1alpha1.SecretReference{Namespace: "pk-metal", Name: "stamp-fleet-secrets"}},
		}
		// The policies reach the API server a moment after their create.
		require.Eventually(t, func() bool {
			try := cluster.DeepCopy()
			if err := alice.Create(ctx, try); err != nil {
				return false
			}
			cluster = try
			return true
		}, 30*time.Second, 100*time.Millisecond, "a create without the annotation is never accepted")
		assert.Equal(t, "alice", named(cluster), "the create names its user")

		require.NoError(t, patch(bob, cluster, `{"spec":{"talosVersion":"v1.14.7"}}`))
		assert.Equal(t, "bob", named(cluster), "a change of the spec names its user")
		require.NoError(t, patch(alice, cluster, `{"metadata":{"labels":{"team":"a"}}}`))
		assert.Equal(t, "bob", named(cluster), "a label is no intent: the name stays")
		require.NoError(t, patch(alice, cluster, `{"metadata":{"annotations":{"`+key+`":"bob"}},"spec":{"talosVersion":"v1.14.8"}}`))
		assert.Equal(t, "alice", named(cluster), "a request names its own user, not the one it writes")
		require.Error(t, patch(bob, cluster, `{"metadata":{"annotations":{"`+key+`":null}}}`), "nobody removes the name")

		err := bob.Delete(ctx, cluster)
		require.Error(t, err, "a delete still names its requester first")
		assert.Contains(t, err.Error(), "first set the annotation")
		require.NoError(t, patch(bob, cluster, `{"metadata":{"annotations":{"`+key+`":"bob"}}}`))
		require.NoError(t, bob.Delete(ctx, cluster))
	})

	t.Run("a Host names its user for a wipe only", func(t *testing.T) {
		host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "stamp-host"}}
		require.NoError(t, cl.Create(ctx, host))
		t.Cleanup(func() { _ = cl.Delete(ctx, host) })
		require.NoError(t, patch(alice, host, `{"metadata":{"labels":{"rack":"r1"}}}`))
		assert.Empty(t, named(host), "a label is no intent")
		require.NoError(t, patch(alice, host, `{"metadata":{"annotations":{"`+metalv1alpha1.AnnotationWipe+`":"v1.14.6"}}}`))
		assert.Equal(t, "alice", named(host))
	})

	t.Run("a Talos config patch names its user", func(t *testing.T) {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "stamp-patch", Namespace: "pk-metal",
			Labels: map[string]string{osv1alpha1.LabelPatch: "true"}}, Data: map[string]string{"patch.yaml": "{}"}}
		require.NoError(t, alice.Create(ctx, cm))
		assert.Equal(t, "alice", named(cm))
		require.NoError(t, patch(bob, cm, `{"data":{"patch.yaml":"machine: {}"}}`))
		assert.Equal(t, "bob", named(cm))
	})

	t.Run("pausing a rollout names who did it", func(t *testing.T) {
		ro := &osv1alpha1.TalosRollout{
			ObjectMeta: metav1.ObjectMeta{Name: "stamp-rollout"},
			Spec: osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: "fleet"},
				Kind: osv1alpha1.RolloutReboot, Nodes: []string{"node-1"}, ProposedBy: "op-sa"},
		}
		require.NoError(t, st.userClient(t, "op-sa").Create(ctx, ro))
		t.Cleanup(func() { _ = cl.Delete(ctx, ro) })
		assert.Empty(t, named(ro), "the operator's create of a rollout is no intent")
		require.NoError(t, patch(alice, ro, `{"spec":{"paused":true}}`))
		assert.Equal(t, "alice", named(ro))
	})
}

// The API server writes the name in front of the reason of a guard lift
// (config/name-stamp/guard-lift-stamp.yaml): a person gives the reason
// only, and the guard-lift policy still finds "<username>: <reason>".
func TestGuardLiftStamp(t *testing.T) {
	st := startEnv(t)
	cl := st.cl
	ctx := t.Context()
	installSettings(t, ctx, cl, "op-gl")
	installPolicy(t, ctx, cl, "guard-lift.yaml")
	installStamp(t, ctx, cl)
	alice := st.userClient(t, "alice")
	lift := func(c client.Client, obj client.Object, key string, value any) error {
		raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: value}}})
		require.NoError(t, err)
		return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw))
	}
	value := func(obj client.Object, key string) string {
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(obj), obj))
		return obj.GetAnnotations()[key]
	}

	op := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "stamp-op", Namespace: "default"},
		Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "no-such-host"}, ProposedBy: "alice",
			Actions: []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}}},
	}
	// Some seconds after a CRD is new, the API server cannot yet type an
	// object of it for the patch of a MutatingAdmissionPolicy, and refuses
	// its write despite failurePolicy Ignore.
	require.Eventually(t, func() bool { return cl.Create(ctx, op.DeepCopy()) == nil },
		30*time.Second, 100*time.Millisecond, "the HostOperation is never created")
	guard := metalv1alpha1.AnnotationOverrideClusterGuard
	// The policies reach the API server a moment after their create.
	require.Eventually(t, func() bool {
		return lift(alice, op, guard, "one control plane") == nil && value(op, guard) == "alice: one control plane"
	}, 30*time.Second, 100*time.Millisecond, "a reason alone never got the name of its writer")

	t.Run("a value in the named form stays as it is", func(t *testing.T) {
		require.NoError(t, lift(alice, op, guard, "alice: etcd is fine"))
		assert.Equal(t, "alice: etcd is fine", value(op, guard))
	})
	t.Run("nobody writes a lift in the name of another person", func(t *testing.T) {
		require.NoError(t, lift(alice, op, guard, "bob: he said so"))
		assert.Equal(t, "alice: bob: he said so", value(op, guard))
	})
	t.Run("an empty value is still refused and a removal still allowed", func(t *testing.T) {
		require.Error(t, lift(alice, op, guard, ""))
		require.NoError(t, lift(alice, op, guard, nil))
		assert.Empty(t, value(op, guard))
	})
	t.Run("a waiver of the wipe names its writer", func(t *testing.T) {
		host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "stamp-host",
			Annotations: map[string]string{metalv1alpha1.AnnotationDiskNotWiped: "true"}}}
		require.NoError(t, cl.Create(ctx, host))
		raw := `{"metadata":{"annotations":{"` + metalv1alpha1.AnnotationDiskNotWiped + `":null,"` +
			metalv1alpha1.AnnotationWipeWaived + `":"the disk is new"}}}`
		require.NoError(t, alice.Patch(ctx, host, client.RawPatch(types.MergePatchType, []byte(raw))))
		assert.Equal(t, "alice: the disk is new", value(host, metalv1alpha1.AnnotationWipeWaived))
	})
	t.Run("an etcd risk on a node names its writer", func(t *testing.T) {
		node := &osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: "stamp-node"},
			Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "lift"}, Role: osv1alpha1.RoleControlPlane,
				Address: "192.0.2.42", InstallDiskSelector: osv1alpha1.DiskSelector{Serial: "disk-2"}},
		}
		require.NoError(t, cl.Create(ctx, node))
		require.NoError(t, lift(alice, node, metalv1alpha1.AnnotationAckEtcdRisk, "two of three stay"))
		assert.Equal(t, "alice: two of three stay", value(node, metalv1alpha1.AnnotationAckEtcdRisk))
	})
}

// The stamps change what the validating policies check, so the two must
// match the same requests and read the same annotations: a kind that one
// lists and the other not would get a stamp that nobody checks, or a check
// that nobody stamps for.
func TestNameStampMatchesTheValidatingPolicies(t *testing.T) {
	validating := func(file, name string) admissionregistrationv1.ValidatingAdmissionPolicySpec {
		for _, doc := range strings.Split(readFile(t, filepath.Join("..", "..", "config", "policy", file)), "\n---") {
			p := admissionregistrationv1.ValidatingAdmissionPolicy{}
			require.NoError(t, yaml.Unmarshal([]byte(doc), &p))
			if p.Kind == "ValidatingAdmissionPolicy" && p.Name == name {
				return p.Spec
			}
		}
		t.Fatalf("no policy %s in %s", name, file)
		return admissionregistrationv1.ValidatingAdmissionPolicySpec{}
	}
	mutating := func(name string) admissionregistrationv1.MutatingAdmissionPolicySpec {
		for _, file := range []string{"requested-by-stamp.yaml", "guard-lift-stamp.yaml"} {
			for _, doc := range strings.Split(readFile(t, filepath.Join("..", "..", "config", "name-stamp", file)), "\n---") {
				p := admissionregistrationv1.MutatingAdmissionPolicy{}
				require.NoError(t, yaml.Unmarshal([]byte(doc), &p))
				if p.Kind == "MutatingAdmissionPolicy" && p.Name == name {
					return p.Spec
				}
			}
		}
		t.Fatalf("no policy %s", name)
		return admissionregistrationv1.MutatingAdmissionPolicySpec{}
	}
	// The group/resource/operation triples, without deletes: a delete
	// takes no stamp.
	matched := func(rules []admissionregistrationv1.NamedRuleWithOperations) []string {
		var out []string
		for _, r := range rules {
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					for _, op := range r.Operations {
						if op != admissionregistrationv1.Delete {
							out = append(out, g+"/"+res+" "+string(op))
						}
					}
				}
			}
		}
		slices.Sort(out)
		return out
	}
	variable := func(vars []admissionregistrationv1.Variable, name string) string {
		for _, v := range vars {
			if v.Name == name {
				return v.Expression
			}
		}
		t.Fatalf("no variable %s", name)
		return ""
	}

	// The matchResources of each binding in a file, by name.
	bindings := func(dir, file string) map[string]*admissionregistrationv1.MatchResources {
		out := map[string]*admissionregistrationv1.MatchResources{}
		for _, doc := range strings.Split(readFile(t, filepath.Join("..", "..", "config", dir, file)), "\n---") {
			var b struct {
				Kind     string            `json:"kind"`
				Metadata metav1.ObjectMeta `json:"metadata"`
				Spec     struct {
					MatchResources *admissionregistrationv1.MatchResources `json:"matchResources"`
				} `json:"spec"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(doc), &b))
			if strings.HasSuffix(b.Kind, "Binding") {
				out[b.Metadata.Name] = b.Spec.MatchResources
			}
		}
		return out
	}

	for _, pair := range []struct{ file, validating, mutating string }{
		{"requested-by.yaml", "requested-by", "requested-by-stamp"},
		{"guard-lift.yaml", "guard-lift", "guard-lift-stamp"},
	} {
		v, m := validating(pair.file, pair.validating), mutating(pair.mutating)
		assert.Equal(t, matched(v.MatchConstraints.ResourceRules), matched(m.MatchConstraints.ResourceRules),
			"%s and %s match other requests", pair.validating, pair.mutating)
		assert.Equal(t, v.MatchConstraints.ObjectSelector, m.MatchConstraints.ObjectSelector, "%s and %s select other objects", pair.validating, pair.mutating)
	}
	// requested-by has a binding of its own for the Talos config patches,
	// and its stamp has the same two.
	vb, mb := bindings("policy", "requested-by.yaml"), bindings("name-stamp", "requested-by-stamp.yaml")
	require.Len(t, mb, len(vb))
	for v, m := range map[string]string{"requested-by": "requested-by-stamp", "requested-by-patches": "requested-by-stamp-patches"} {
		require.NotNil(t, vb[v], v)
		require.NotNil(t, mb[m], m)
		assert.Equal(t, matched(vb[v].ResourceRules), matched(mb[m].ResourceRules), "%s and %s match other requests", v, m)
		assert.Equal(t, matched(vb[v].ExcludeResourceRules), matched(mb[m].ExcludeResourceRules), "%s and %s leave out other requests", v, m)
		assert.Equal(t, vb[v].ObjectSelector, mb[m].ObjectSelector, "%s and %s select other objects", v, m)
	}
	rb, rbs := validating("requested-by.yaml", "requested-by"), mutating("requested-by-stamp")
	assert.Equal(t, variable(rb.Variables, "setAnnotation"), variable(rbs.Variables, "setAnnotation"))
	gl, gls := validating("guard-lift.yaml", "guard-lift"), mutating("guard-lift-stamp")
	assert.Equal(t, variable(gl.Variables, "lifts"), variable(gls.Variables, "lifts"))
	quoted := regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(variable(gl.Variables, "lifts"), -1)
	var lifts []string
	for _, q := range quoted {
		lifts = append(lifts, q[1])
	}
	assert.ElementsMatch(t, metalv1alpha1.NamedGuardLifts, lifts, "guard-lift.yaml and the API list other lifts")
}

// installStamp creates the committed MutatingAdmissionPolicies of
// config/name-stamp and their bindings.
func installStamp(t *testing.T, ctx context.Context, cl client.Client) {
	t.Helper()
	for _, file := range []string{"requested-by-stamp.yaml", "guard-lift-stamp.yaml"} {
		for _, doc := range strings.Split(readFile(t, filepath.Join("..", "..", "config", "name-stamp", file)), "\n---") {
			u := &unstructured.Unstructured{}
			require.NoError(t, yaml.Unmarshal([]byte(doc), &u.Object))
			require.NoError(t, cl.Create(ctx, u))
		}
	}
}
