package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNamedLift(t *testing.T) {
	ctx := context.Background()
	cl := fakeClientAs("alice")
	got, err := namedLift(ctx, cl, " disk swap ")
	require.NoError(t, err)
	assert.Equal(t, "alice: disk swap", got, "the caller's username, then the reason")

	got, err = namedLift(ctx, cl, "alice: disk swap")
	require.NoError(t, err)
	assert.Equal(t, "alice: disk swap", got, "a value that names the caller already is not named twice")

	got, err = namedLift(ctx, cl, "  ")
	require.NoError(t, err)
	assert.Empty(t, got, "no reason, no lift")

	broken := fake.NewClientBuilder().WithScheme(newScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return errors.New("forbidden")
		},
	}).Build()
	_, err = namedLift(ctx, broken, "disk swap")
	assert.ErrorContains(t, err, "the guard lift must name you")
}
