package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// backupSnapshot is a finished snapshot record of policy p, taken at at.
// A key names a stored object; a failed snapshot without a key failed
// before it streamed.
func backupSnapshot(name string, at time.Time, verified bool, key string) *osv1alpha1.EtcdSnapshot {
	complete, verifiedStatus := metav1.ConditionTrue, metav1.ConditionTrue
	if !verified {
		complete, verifiedStatus = metav1.ConditionFalse, metav1.ConditionFalse
	}
	return &osv1alpha1.EtcdSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{osv1alpha1.LabelSnapshotPolicy: "p"}},
		Spec:       osv1alpha1.EtcdSnapshotSpec{PolicyRef: osv1alpha1.Reference{Name: "p"}},
		Status: osv1alpha1.EtcdSnapshotStatus{
			TakenAt: &metav1.Time{Time: at}, Key: key,
			Conditions: []metav1.Condition{
				{Type: osv1alpha1.ConditionComplete, Status: complete, Reason: "Test", LastTransitionTime: metav1.Now()},
				{Type: osv1alpha1.ConditionVerified, Status: verifiedStatus, Reason: "Test", LastTransitionTime: metav1.Now()},
			},
		},
	}
}

// A sink that refuses deletes (a key without delete rights, a read-only
// volume) gives one error line for each pass, not one for each expired
// snapshot, and the records that have no stored object are still pruned.
func TestRetentionPausesOnARefusedDelete(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	policy := &osv1alpha1.EtcdBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: osv1alpha1.EtcdBackupPolicySpec{
			ClusterRef: osv1alpha1.Reference{Name: "c"},
			Interval:   metav1.Duration{Duration: time.Hour},
			Retention:  osv1alpha1.EtcdBackupRetention{Count: 1},
			Sink:       osv1alpha1.EtcdBackupSink{PVC: &osv1alpha1.PVCSink{}},
			Encryption: osv1alpha1.EtcdBackupEncryption{AgeRecipients: []string{id.Recipient().String()}},
		},
	}
	objs := []client.Object{policy, &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}}}
	dir := t.TempDir()
	// Four verified snapshots, the oldest first: the newest stays, and the
	// sink refuses to delete the other three. A non-empty directory at the
	// key makes the delete fail, also for root.
	for i := range 4 {
		name := "v" + string(rune('1'+i))
		key := "c/" + name + ".db.age"
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "c", name+".db.age", "locked"), 0o700))
		objs = append(objs, backupSnapshot(name, now.Add(time.Duration(i-8)*time.Hour), true, key))
	}
	// Four failed snapshots without a key, newer than the verified ones:
	// the oldest of them is one too many (keepFailedSnapshots).
	for i := range 4 {
		objs = append(objs, backupSnapshot("f"+string(rune('1'+i)), now.Add(time.Duration(i-4)*time.Hour), false, ""))
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&osv1alpha1.EtcdBackupPolicy{}).WithObjects(objs...).Build()
	r := &EtcdBackupPolicyReconciler{&EtcdBackups{Client: cl, Dir: dir, Now: func() time.Time { return now }}}

	var errorLines []string
	ctx := logf.IntoContext(t.Context(), funcr.New(func(_, args string) {
		if strings.Contains(args, `"error"=`) {
			errorLines = append(errorLines, args)
		}
	}, funcr.Options{}))
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
	require.NoError(t, err)

	assert.Len(t, errorLines, 1, "one line for the pass, not one for each snapshot")
	for _, name := range []string{"v1", "v2", "v3", "v4"} {
		assert.NoError(t, cl.Get(ctx, types.NamespacedName{Name: name}, &osv1alpha1.EtcdSnapshot{}),
			"%s keeps its record while its stored object stays", name)
	}
	err = cl.Get(ctx, types.NamespacedName{Name: "f1"}, &osv1alpha1.EtcdSnapshot{})
	assert.True(t, apierrors.IsNotFound(err), "a failed snapshot without a stored object is pruned: %v", err)
}
