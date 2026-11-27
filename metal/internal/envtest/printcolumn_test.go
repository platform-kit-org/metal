package envtest_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// kubectl get maintenancewindows shows when a window opens or closes next.
// The API server prints a date column as the age of its time, and a time in
// the future has no age: the column showed <invalid>.
func TestNextChangeColumnShowsTheTime(t *testing.T) {
	st := startEnv(t)
	cl, ctx := st.cl, t.Context()

	w := &metalv1alpha1.MaintenanceWindow{
		ObjectMeta: metav1.ObjectMeta{Name: "weekend"},
		Spec: metalv1alpha1.MaintenanceWindowSpec{
			HostSelector: &metav1.LabelSelector{},
			Weekly: []metalv1alpha1.WeeklyWindow{{Days: []metalv1alpha1.Weekday{"Sat"}, Start: "22:00",
				Duration: metav1.Duration{Duration: 4 * time.Hour}}},
		},
	}
	require.NoError(t, cl.Create(ctx, w))
	next := metav1.NewTime(time.Now().Add(48 * time.Hour).Truncate(time.Second))
	w.Status.NextChange = &next
	require.NoError(t, cl.Status().Update(ctx, w))

	cs, err := kubernetes.NewForConfig(st.cfg)
	require.NoError(t, err)
	body, err := cs.Discovery().RESTClient().Get().
		AbsPath("/apis/metal.platform-kit.org/v1alpha1/maintenancewindows/weekend").
		SetHeader("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io").DoRaw(ctx)
	require.NoError(t, err)
	table := metav1.Table{}
	require.NoError(t, json.Unmarshal(body, &table))
	require.Len(t, table.Rows, 1)
	for i, c := range table.ColumnDefinitions {
		if c.Name == "Next change" {
			assert.Equal(t, next.UTC().Format(time.RFC3339), table.Rows[0].Cells[i])
			return
		}
	}
	t.Fatal("no column Next change")
}
