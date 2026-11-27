package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// fakeClientAs is a fake API that answers a SelfSubjectReview with user, as
// the API server answers it for the caller's credential.
func fakeClientAs(user string, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if ssr, ok := obj.(*authenticationv1.SelfSubjectReview); ok {
					ssr.Status.UserInfo.Username = user
					return nil
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
}

func TestRequestedBy(t *testing.T) {
	assert.Equal(t, map[string]any{metalv1alpha1.AnnotationRequestedBy: "alice"}, requestedBy("alice", "  "),
		"no reason, no lift")
	for _, ack := range []string{" disk swap ", "alice: disk swap"} {
		assert.Equal(t, map[string]any{metalv1alpha1.AnnotationRequestedBy: "alice",
			metalv1alpha1.AnnotationAckEtcdRisk: "alice: disk swap"}, requestedBy("alice", ack),
			"the caller's username, then the reason, and not twice")
	}
}

func TestWhoAmI(t *testing.T) {
	ctx := context.Background()
	user, err := whoAmI(ctx, fakeClientAs("alice"))
	require.NoError(t, err)
	assert.Equal(t, "alice", user)

	broken := fake.NewClientBuilder().WithScheme(newScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return errors.New("forbidden")
		},
	}).Build()
	_, err = whoAmI(ctx, broken)
	assert.ErrorContains(t, err, "cannot ask the cluster who you are (pk-metal records your name on each change): forbidden")
}
