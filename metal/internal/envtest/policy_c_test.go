package envtest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

const (
	adminGroup = "pk-metal:admins"
	recipientA = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"
	recipientB = "age1lggyhqrw2nlhcxprm67z43rta597azn8gknawjehu9d9dl0jq3yqqvfafg"
)

// refusedOnceEnforcing waits until a policy compiles and refuses c's create
// of obj with a message that contains want. A policy takes a moment to become
// enforcing after its creation; a create that passes in that moment is
// deleted again, so the next try starts clean.
func refusedOnceEnforcing(t *testing.T, ctx context.Context, admin, c client.Client, obj client.Object, want string) error {
	t.Helper()
	var last error
	require.Eventually(t, func() bool {
		try := obj.DeepCopyObject().(client.Object)
		last = c.Create(ctx, try)
		if last == nil {
			require.NoError(t, admin.Delete(ctx, try))
			return false
		}
		return strings.Contains(last.Error(), want)
	}, 30*time.Second, 100*time.Millisecond, "the policy never refused with %q (last: %v)", want, last)
	return last
}

func TestPolicyStreamC(t *testing.T) {
	st := startEnv(t)
	ctx := t.Context()
	cl := st.cl

	installApprovalPolicy(t, ctx, cl, "op-sa")
	installPolicy(t, ctx, cl, "hostclaim-provisioning.yaml", "etcdbackuppolicy-guard.yaml", "endpointprofile-guard.yaml")

	// userClient adds system:masters: these clients test admission, not RBAC.
	alice := st.userClient(t, "alice")
	ada := st.userClient(t, "ada", adminGroup)

	t.Run("the claim refusal says where group membership comes from", func(t *testing.T) {
		claim := &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "c4", Namespace: "default"},
			Spec: metalv1alpha1.HostClaimSpec{
				AssetRef: metalv1alpha1.Reference{Name: "a1"},
				OS:       &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: "n1"}},
			},
		}
		err := refusedOnceEnforcing(t, ctx, cl, alice, claim, "only an admin")
		assert.Contains(t, err.Error(), "not from a binding")
		assert.Contains(t, err.Error(), "O= of your client certificate")
		assert.NotContains(t, err.Error(), "ClusterRoleBinding to the group")
	})

	t.Run("only an admin decides who reads the backups and where they go", func(t *testing.T) {
		p := pvcPolicy("c3", "prod", recipientA, 3)
		err := refusedOnceEnforcing(t, ctx, cl, alice, p, "only an admin")
		assert.Contains(t, err.Error(), "encryption or sink")
		require.NoError(t, ada.Create(ctx, p.DeepCopy()))

		mine := client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"encryption":{"ageRecipients":["`+recipientA+`","`+recipientB+`"]}}}`))
		err = alice.Patch(ctx, p.DeepCopy(), mine)
		require.Error(t, err, "a non-admin must not add a recipient")
		assert.Contains(t, err.Error(), "only an admin")

		sink := client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"sink":{"pvc":null,"s3":{"endpoint":"https://s3.example","bucket":"mine",`+
				`"credentialsSecretRef":{"name":"mine"}}}}}`))
		err = alice.Patch(ctx, p.DeepCopy(), sink)
		require.Error(t, err, "a non-admin must not move the sink")
		assert.Contains(t, err.Error(), "only an admin")

		interval := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"interval":"2h","suspend":true}}`))
		require.NoError(t, alice.Patch(ctx, p.DeepCopy(), interval), "interval and suspend stay open")
		require.NoError(t, ada.Patch(ctx, p.DeepCopy(), mine), "an admin adds a recipient")
	})

	t.Run("an http S3 endpoint needs allowPlainHTTP", func(t *testing.T) {
		s3 := func(name, endpoint string, plain bool) *osv1alpha1.EtcdBackupPolicy {
			p := pvcPolicy(name, "prod", recipientA, 3)
			p.Spec.Sink = osv1alpha1.EtcdBackupSink{S3: &osv1alpha1.S3Sink{
				Endpoint: endpoint, Bucket: "backups", AllowPlainHTTP: plain,
				CredentialsSecretRef: osv1alpha1.Reference{Name: "s3"},
			}}
			return p
		}
		err := ada.Create(ctx, s3("c3-http", "http://minio.lab:9000", false))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "endpoint must be https://")
		require.NoError(t, ada.Create(ctx, s3("c3-http-lab", "http://minio.lab:9000", true)))
		require.NoError(t, ada.Create(ctx, s3("c3-https", "https://s3.example", false)))
	})

	t.Run("only an admin creates or changes an EndpointProfile", func(t *testing.T) {
		ep := &metalv1alpha1.EndpointProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "c10"},
			Spec: metalv1alpha1.EndpointProfileSpec{
				NTP: &metalv1alpha1.NTPPolicy{Servers: []string{"ntp1.example"}},
			},
		}
		err := refusedOnceEnforcing(t, ctx, cl, alice, ep, "only an admin")
		assert.Contains(t, err.Error(), "EndpointProfile")
		require.NoError(t, ada.Create(ctx, ep.DeepCopy()))

		issuer := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"tls":{"issuerRef":{"name":"mine"}}}}`))
		err = alice.Patch(ctx, ep.DeepCopy(), issuer)
		require.Error(t, err, "a non-admin must not name the issuer of the BMC identities")
		assert.Contains(t, err.Error(), "only an admin")
		ntp := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"ntp":{"servers":["ntp.evil.example"]}}}`))
		require.Error(t, alice.Patch(ctx, ep.DeepCopy(), ntp))

		label := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"team":"a"}}}`))
		require.NoError(t, alice.Patch(ctx, ep.DeepCopy(), label), "metadata stays open")
		require.NoError(t, ada.Patch(ctx, ep.DeepCopy(), ntp), "an admin changes the profile")
	})
}

// TestOnCallRole: the committed on-call role covers what the runbooks do,
// and holds neither the approval (patch on operations and rollouts) nor any
// Secret.
func TestOnCallRole(t *testing.T) {
	st := startEnv(t)
	ctx := t.Context()
	cl := st.cl

	clusterRoles, roles, _, _ := rbacDocs(t, "oncall_role.yaml")
	require.Len(t, clusterRoles, 1)
	role := clusterRoles[0]
	require.Equal(t, "pk-metal-oncall", role.Name)
	require.NoError(t, cl.Create(ctx, &role))
	require.NoError(t, cl.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "pk-metal-oncall-bob"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "bob"}},
	}))
	require.Len(t, roles, 1)
	ensureNamespace(t, ctx, cl, roles[0].Namespace)
	require.NoError(t, cl.Create(ctx, &roles[0]))
	require.NoError(t, cl.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "pk-metal-oncall-bob", Namespace: roles[0].Namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: roles[0].Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "bob"}},
	}))

	allowedIn := func(ns, verb, group, resource, sub string) bool {
		sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: "bob", Groups: []string{"system:authenticated"},
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb: verb, Group: group, Resource: resource, Subresource: sub, Namespace: ns,
			},
		}}
		require.NoError(t, cl.Create(ctx, sar))
		return sar.Status.Allowed
	}
	allowed := func(verb, group, resource string) bool { return allowedIn("pk-metal", verb, group, resource, "") }
	for _, c := range []struct {
		verb, group, resource string
		want                  bool
		why                   string
	}{
		{"delete", "metal.platform-kit.org", "hostoperations", true, "pk-metal retry"},
		{"patch", "metal.platform-kit.org", "hostclaims", true, "pk-metal hold, unhold"},
		{"patch", "metal.platform-kit.org", "hosts", true, "pk-metal wipe, the disk-not-wiped mark"},
		{"patch", "metal.platform-kit.org", "assets", true, "correct the ledger facts"},
		{"patch", "os.platform-kit.org", "talosclusters", true, "remove restore-pending"},
		{"delete", "os.platform-kit.org", "talosrollouts", true, "stop a rollout for good"},
		{"list", "metal.platform-kit.org", "managementendpoints", true, "describe"},
		{"list", "os.platform-kit.org", "etcdsnapshots", true, "backup-failing"},
		{"list", "", "events", true, "describe"},
		{"patch", "metal.platform-kit.org", "hostoperations", false, "patch is approval"},
		{"patch", "os.platform-kit.org", "talosrollouts", false, "patch is approval"},
		{"create", "metal.platform-kit.org", "hostoperations", false, "proposing is the operator's"},
		{"patch", "metal.platform-kit.org", "endpointprofiles", false, "profiles are an admin's"},
		{"patch", "os.platform-kit.org", "etcdbackuppolicies", false, "backup policies are an admin's"},
		{"get", "", "secrets", false, "no Secret"},
		{"list", "", "pods", true, "management-cluster-lost: kubectl get pods"},
		{"list", "", "endpoints", true, "management-cluster-lost: kubectl get endpoints"},
		{"get", "apps", "deployments", true, "kubectl logs deploy/pk-metal-manager"},
		{"list", "batch", "jobs", true, "the imager Jobs"},
		{"list", "admissionregistration.k8s.io", "validatingadmissionpolicies", true, "admission-gate-degraded"},
		{"list", "admissionregistration.k8s.io", "validatingadmissionpolicybindings", true, "admission-gate-degraded"},
		{"patch", "admissionregistration.k8s.io", "validatingadmissionpolicybindings", false, "a policy is an admin's"},
		{"delete", "", "pods", false, "restarting the manager is an admin's"},
	} {
		assert.Equal(t, c.want, allowed(c.verb, c.group, c.resource), "%s %s/%s (%s)", c.verb, c.group, c.resource, c.why)
	}
	assert.True(t, allowedIn("pk-metal", "get", "", "pods", "log"), "the runbooks read the manager log")
	assert.False(t, allowedIn("kube-system", "get", "", "pods", "log"), "the log of other namespaces stays closed")
	assert.False(t, allowedIn("kube-system", "list", "", "pods", ""), "the pods of other namespaces stay closed")

	// The manager's /metrics, for a look when Prometheus is down.
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: "bob", Groups: []string{"system:authenticated"},
		NonResourceAttributes: &authorizationv1.NonResourceAttributes{Verb: "get", Path: "/metrics"},
	}}
	require.NoError(t, cl.Create(ctx, sar))
	assert.True(t, sar.Status.Allowed, "get /metrics")
}
