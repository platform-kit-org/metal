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
	"github.com/dennisklein/platform-kit/metal/internal/talos"
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

// The snapshot comes off a healthy follower first, the first by name, and
// off the leader only when no follower is healthy. A member that no
// control-plane TalosNode describes is never picked.
func TestPickMember(t *testing.T) {
	var cps []osv1alpha1.TalosNode
	for _, n := range []string{"cp-1", "cp-2", "cp-3"} {
		cps = append(cps, osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: n}, Spec: osv1alpha1.TalosNodeSpec{Address: "10.0.0." + n[3:]}})
	}
	report := func(sick ...uint64) *etcdReport {
		rep := &etcdReport{leader: 1, sick: map[uint64]string{}, learners: map[uint64]bool{},
			members: []talos.EtcdMember{{ID: 4, Hostname: "cp-0"}, {ID: 3, Hostname: "cp-3"}, {ID: 2, Hostname: "cp-2"}, {ID: 1, Hostname: "cp-1"}}}
		for _, id := range sick {
			rep.sick[id] = "knows no leader"
		}
		return rep
	}
	for name, tc := range map[string]struct {
		rep  *etcdReport
		want string
	}{
		"a follower, the first by name":                  {rep: report(), want: "cp-2"},
		"the next healthy follower":                      {rep: report(2), want: "cp-3"},
		"the leader, when no follower is healthy":        {rep: report(2, 3), want: "cp-1"},
		"no healthy member":                              {rep: report(1, 2, 3, 4)},
		"only a healthy member that no TalosNode is for": {rep: report(1, 2, 3)},
	} {
		t.Run(name, func(t *testing.T) {
			src, why := pickMember(tc.rep, cps)
			if tc.want == "" {
				require.Nil(t, src)
				assert.Contains(t, why, "member cp-1 (1) knows no leader")
				return
			}
			require.NotNil(t, src, why)
			assert.Equal(t, tc.want, src.node)
			assert.Equal(t, "10.0.0."+tc.want[3:], src.endpoint)
		})
	}
}
