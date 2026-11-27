package controller

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
)

// The clients below answer reads as an informer cache that has not caught up
// with the store yet; writes go to the store. Give the reconciler one of them
// as Client, and the store as APIReader. A test of a stale-cache fix asserts
// the safe outcome, then sets APIReader to nil and sees it go.

// staleGet answers each Get of a T from the store, then old turns the object
// into what the cache shows, or returns the error the cache gives.
func staleGet[T client.Object](live client.Client, old func(key client.ObjectKey, obj T) error) client.Client {
	return interceptor.NewClient(live.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if o, ok := obj.(T); ok {
				return old(key, o)
			}
			return nil
		},
	})
}

// staleList answers each List of an L from the store, then old turns the
// list into what the cache shows.
func staleList[L client.ObjectList](live client.Client, old func(list L)) client.Client {
	return interceptor.NewClient(live.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if l, ok := list.(L); ok {
				old(l)
			}
			return nil
		},
	})
}

// staleLists answers every list from a second store, the cache, which holds
// older objects.
type staleLists struct {
	client.Client
	cache client.Client
}

func (c *staleLists) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.cache.List(ctx, list, opts...)
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
	g.r.Client = staleGet(g.cl, func(k client.ObjectKey, sec *corev1.Secret) error {
		if k == key {
			before.DeepCopyInto(sec)
		}
		return nil
	})
	g.r.APIReader = g.cl
	status, reason, msg := g.pass(t)
	assert.Equal(t, "True", string(status), "%s: %s", reason, msg)
	assert.True(t, g.state.Authenticate("pk-metal", promoted), "the promoted password still logs in")
	assert.Equal(t, failed, g.d.failedLogins, "no login with the old password")
	assert.NotContains(t, trail.String(), "Repaired", "nothing is repaired")
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
	r.Client = staleGet(s.cl, func(k client.ObjectKey, _ *corev1.Secret) error {
		if k == key {
			return apierrors.NewNotFound(corev1.Resource("secrets"), k.Name)
		}
		return nil
	})
	_, again, err := r.ensureIdentityInFlight(t.Context(), s.ep, desired, hash, bmc.DialectCSRNative)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the manager makes no second request")
	assert.Equal(t, first, again, "the stored request is the one the manager holds the key of")
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

	cache := staleList(api, func(l *metalv1alpha1.HostClaimList) { l.Items = []metalv1alpha1.HostClaim{*claim} })
	r := &TalosNodeMemberReconciler{Client: cache, APIReader: api, OperationsNamespace: "pk-metal"}
	reason, _, err := r.blocked(t.Context(), node)
	require.NoError(t, err)
	assert.Equal(t, osv1alpha1.ReasonMachineAssigned, reason, "the claim's record of the install holds the node")

	r.APIReader = nil
	reason, _, err = r.blocked(t.Context(), node)
	require.NoError(t, err)
	assert.Empty(t, reason, "from the stale cache alone, the node would go")
}

// The member finalizer of a control plane waits while etcd lists the node.
// It reads the cluster from the API server: a cache from before the
// bootstrap would skip the etcd check, and the member would stay in etcd
// with no node left to remove it.
func TestMemberFinalizerReadsTheClusterFromTheAPIServer(t *testing.T) {
	lab := newLifecycleLab(t, []string{"cp-1", "cp-2"}, nil)
	node := &osv1alpha1.TalosNode{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "cp-2"}, node))
	cache := staleGet(lab.r.Client, func(_ client.ObjectKey, c *osv1alpha1.TalosCluster) error {
		c.Status.BootstrappedAt = nil // from before the bootstrap
		return nil
	})
	r := &TalosNodeMemberReconciler{Client: cache, APIReader: lab.r.Client,
		Dial: lab.fleet.Dialer(), CredentialsNamespace: "pk-metal", OperationsNamespace: "pk-metal"}

	reason, _, err := r.blocked(t.Context(), node)
	require.NoError(t, err)
	assert.Equal(t, osv1alpha1.ReasonEtcdMemberRemains, reason)

	r.APIReader = nil
	reason, _, err = r.blocked(t.Context(), node)
	require.NoError(t, err)
	assert.Empty(t, reason, "from the stale cache alone, the node would go")
}
