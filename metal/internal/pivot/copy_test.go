package pivot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The first write of a kind right after Install can meet a mutating
// admission policy that does not know the kind yet. The move writes again,
// and stops at any other error.
func TestCopyWaitsUntilAdmissionKnowsTheKind(t *testing.T) {
	m := &Mover{Target: &Cluster{Name: "target"}, Options: Options{Timeout: 5 * time.Second, Poll: time.Millisecond}}
	notYet := errors.New("Resource kind metal.platform-kit.org/v1alpha1, Kind=Host not found. " + kindNotYetKnown + ".")

	calls := 0
	require.NoError(t, m.untilAdmitted(t.Context(), func() error {
		calls++
		if calls < 3 {
			return notYet
		}
		return nil
	}))
	assert.Equal(t, 3, calls)

	refused := errors.New("admission webhook denied the request")
	calls = 0
	err := m.untilAdmitted(t.Context(), func() error { calls++; return refused })
	require.ErrorIs(t, err, refused)
	assert.Equal(t, 1, calls, "another error is not retried")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Error(t, m.untilAdmitted(ctx, func() error { return notYet }), "it gives up")
}
