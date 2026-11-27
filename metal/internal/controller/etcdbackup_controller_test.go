package controller

import (
	"errors"
	"testing"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// The policy's Ready condition and each snapshot read the same checks, in
// order. A failed check keeps what the checks before it read: retention
// still prunes with the sink.
func TestBackupTargetChecksInOrder(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	policy := func(recipient string) *osv1alpha1.EtcdBackupPolicy {
		return &osv1alpha1.EtcdBackupPolicy{Spec: osv1alpha1.EtcdBackupPolicySpec{
			ClusterRef: osv1alpha1.Reference{Name: "lab"},
			Sink:       osv1alpha1.EtcdBackupSink{PVC: &osv1alpha1.PVCSink{}},
			Encryption: osv1alpha1.EtcdBackupEncryption{AgeRecipients: []string{recipient}},
		}}
	}
	reason := func(err error) string {
		var se *sinkError
		if errors.As(err, &se) {
			return se.reason
		}
		return ""
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	b := &EtcdBackups{Client: cl, Dir: t.TempDir(), VolumeName: "backups"}
	ctx := t.Context()

	_, err = (&EtcdBackups{Client: cl}).target(ctx, policy(id.Recipient().String()))
	assert.Equal(t, osv1alpha1.ReasonSinkUnavailable, reason(err), "no backup volume")

	got, err := b.target(ctx, policy("not a key"))
	assert.Equal(t, osv1alpha1.ReasonRecipientsInvalid, reason(err))
	assert.NotNil(t, got.sink)

	got, err = b.target(ctx, policy(id.Recipient().String()))
	assert.Equal(t, osv1alpha1.ReasonClusterNotFound, reason(err))
	assert.NotNil(t, got.sink)
	assert.Len(t, got.recipients, 1)

	require.NoError(t, cl.Create(ctx, &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "lab"}}))
	got, err = b.target(ctx, policy(id.Recipient().String()))
	require.NoError(t, err)
	assert.Equal(t, "lab/", got.prefix)
	assert.Equal(t, "lab", got.cluster.Name)
}
