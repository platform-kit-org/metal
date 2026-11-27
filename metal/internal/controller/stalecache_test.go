package controller

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
)

// staleSecrets is a client whose reads of the named Secrets answer from an
// older snapshot, the way an informer cache that has not caught up yet
// answers: nil in the map is a Secret the cache has not seen at all. Writes
// go through to the store.
type staleSecrets struct {
	client.Client
	old map[types.NamespacedName]*corev1.Secret
}

func (c staleSecrets) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if sec, ok := obj.(*corev1.Secret); ok {
		if snap, stale := c.old[key]; stale {
			if snap == nil {
				return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
			}
			snap.DeepCopyInto(sec)
			return nil
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// A pass that reads a managed Secret older than the last promotion must
// not put the old password back. Before, the cache's old password failed
// to log in, the pass took that as a password changed behind its back,
// set it again through the factory session, and the stored password
// stopped working until the next pass repaired it once more.
func TestRotationReadsTheManagedSecretFromTheAPIServer(t *testing.T) {
	g := newRotationRig(t)
	g.pass(t) // adoption
	key := types.NamespacedName{Namespace: "pk-metal", Name: managedCredsName(g.ep.Name)}
	before := g.secret(t)
	g.age(t)
	g.pass(t) // rotation: parked, set, promoted
	promoted := string(g.secret(t).Data["password"])
	require.NotEqual(t, string(before.Data["password"]), promoted)
	failed := g.d.failedLogins

	var trail bytes.Buffer
	g.r.Audit = audit.New(&trail, testOperator)
	g.r.Client = staleSecrets{Client: g.cl, old: map[types.NamespacedName]*corev1.Secret{key: before}}
	g.r.APIReader = g.cl
	status, reason, msg := g.pass(t)
	assert.Equal(t, "True", string(status), "%s: %s", reason, msg)
	assert.True(t, g.state.Authenticate("pk-metal", promoted), "the promoted password still logs in")
	assert.Equal(t, failed, g.d.failedLogins, "no login with the old password")
	assert.NotContains(t, trail.String(), "Repaired", "nothing is repaired")
}

// The lanes after a promotion in the same pass dial with the promoted
// password. Before, the ntp lane read the managed Secret from a cache that
// had not seen the promotion yet, and the manager refused the replaced
// password once on each rotation.
func TestLanesAfterAPromotionReadTheManagedSecretFromTheAPIServer(t *testing.T) {
	g := newRotationRig(t)
	g.pass(t) // adoption
	key := types.NamespacedName{Namespace: "pk-metal", Name: managedCredsName(g.ep.Name)}
	before := g.secret(t)
	g.age(t)
	failed := g.d.failedLogins
	g.r.Client = staleSecrets{Client: g.cl, old: map[types.NamespacedName]*corev1.Secret{key: before}}
	g.r.APIReader = g.cl
	meta.SetStatusCondition(&g.ep.Status.Conditions, metav1.Condition{
		Type: metalv1alpha1.ConditionVerified, Status: metav1.ConditionTrue, Reason: "Matched"})

	status, reason, msg, err := g.r.converge(t.Context(), g.ep, []metalv1alpha1.EndpointProfile{{
		ObjectMeta: metav1.ObjectMeta{Name: "site"},
		Spec:       metalv1alpha1.EndpointProfileSpec{Credentials: g.pol, NTP: &metalv1alpha1.NTPPolicy{Servers: []string{"192.0.2.10"}}},
	}})

	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, status, "%s: %s", reason, msg)
	assert.NotEqual(t, string(before.Data["password"]), string(g.secret(t).Data["password"]), "the pass rotated")
	assert.Equal(t, failed, g.d.failedLogins, "no login with the replaced password")
}

// csrCounter counts the requests a csr-native manager is asked to make.
type csrCounter struct {
	bmc.Client
	n *int
}

func (c csrCounter) GenerateCSR(ctx context.Context, s bmc.CSRSubject) ([]byte, error) {
	*c.n++
	return c.Client.GenerateCSR(ctx, s)
}

// A csr-native manager keeps the key of the last request it made only. A
// pass whose cache has not yet seen the Secret of the request made a moment
// ago must not have the manager make another: the certificate issued for
// the stored request would then not match the key the manager holds, and
// every push after it would fail.
func TestIdentityInFlightIsReadFromTheAPIServer(t *testing.T) {
	s := newSimEndpoint(t, "huawei-2288h-v5")
	s.ep.Status.Capabilities = &metalv1alpha1.EndpointCapabilities{IdentityDialect: string(bmc.DialectCSRNative)}
	n := 0
	r := &EndpointConvergenceReconciler{Client: s.cl, APIReader: s.cl, CredentialsNamespace: "pk-metal",
		Dial: func(ctx context.Context, cfg gofishclient.Config) (bmc.Client, error) {
			cl, err := s.dial(ctx, cfg)
			return csrCounter{Client: cl, n: &n}, err
		}}
	pol := &metalv1alpha1.IdentityPolicy{IssuerRef: metalv1alpha1.IssuerRef{Name: "ca"},
		Subject: &metalv1alpha1.CertificateSubject{Country: "DE"}}
	desired := desiredIdentity(s.ep, pol)
	hash := planHash(desired)

	_, first, err := r.ensureIdentityInFlight(t.Context(), s.ep, desired, hash, bmc.DialectCSRNative)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	key := types.NamespacedName{Namespace: "pk-metal", Name: identitySecretName(s.ep.Name)}
	r.Client = staleSecrets{Client: s.cl, old: map[types.NamespacedName]*corev1.Secret{key: nil}}
	_, again, err := r.ensureIdentityInFlight(t.Context(), s.ep, desired, hash, bmc.DialectCSRNative)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the manager makes no second request")
	assert.Equal(t, first, again, "the stored request is the one the manager holds the key of")
}

// staleClaims is a client whose list of claims answers from an older
// snapshot, as an informer cache that has not caught up yet.
type staleClaims struct {
	client.Client
	old []metalv1alpha1.HostClaim
}

func (c staleClaims) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if l, ok := list.(*metalv1alpha1.HostClaimList); ok {
		l.Items = slices.Clone(c.old)
		return nil
	}
	return c.Client.List(ctx, list, opts...)
}

// The member finalizer is released once, and the node is gone for good. It
// reads the claims from the API server: a cache that has not yet seen a
// claim record its install of the node would let the node go while its
// machine runs it.
func TestMemberFinalizerReadsTheClaimsFromTheAPIServer(t *testing.T) {
	s := testScheme(t)
	now := metav1.Now()
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "w-1", Finalizers: []string{talosNodeMemberFinalizer},
		DeletionTimestamp: &now}, Spec: osv1alpha1.TalosNodeSpec{Role: osv1alpha1.RoleWorker}}
	claim := &metalv1alpha1.HostClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "w-1"}}
	installed := claim.DeepCopy()
	installed.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "box-1", Node: "w-1"}
	api := fake.NewClientBuilder().WithScheme(s).WithObjects(node, installed).WithStatusSubresource(node, installed).Build()

	r := &TalosNodeMemberReconciler{Client: staleClaims{Client: api, old: []metalv1alpha1.HostClaim{*claim}},
		APIReader: api, OperationsNamespace: "pk-metal"}
	reason, _, err := r.blocked(t.Context(), node)
	require.NoError(t, err)
	assert.Equal(t, osv1alpha1.ReasonMachineAssigned, reason, "the claim's record of the install holds the node")

	r.APIReader = nil
	reason, _, err = r.blocked(t.Context(), node)
	require.NoError(t, err)
	assert.Empty(t, reason, "from the stale cache alone, the node would go")
}

// staleBootstrap answers reads of a TalosCluster as a cache from before the
// bootstrap does.
type staleBootstrap struct{ client.Client }

func (c staleBootstrap) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if cl, ok := obj.(*osv1alpha1.TalosCluster); ok {
		cl.Status.BootstrappedAt = nil
	}
	return nil
}

// The member finalizer of a control plane waits while etcd lists the node.
// It reads the cluster from the API server: a cache from before the
// bootstrap would skip the etcd check, and the member would stay in etcd
// with no node left to remove it.
func TestMemberFinalizerReadsTheClusterFromTheAPIServer(t *testing.T) {
	lab := newLifecycleLab(t, []string{"cp-1", "cp-2"}, nil)
	node := &osv1alpha1.TalosNode{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "cp-2"}, node))
	r := &TalosNodeMemberReconciler{Client: staleBootstrap{lab.r.Client}, APIReader: lab.r.Client,
		Dial: lab.fleet.Dialer(), CredentialsNamespace: "pk-metal", OperationsNamespace: "pk-metal"}

	reason, _, err := r.blocked(t.Context(), node)
	require.NoError(t, err)
	assert.Equal(t, osv1alpha1.ReasonEtcdMemberRemains, reason)

	r.APIReader = nil
	reason, _, err = r.blocked(t.Context(), node)
	require.NoError(t, err)
	assert.Empty(t, reason, "from the stale cache alone, the node would go")
}
