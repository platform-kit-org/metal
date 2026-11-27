// The roles the operator ships, and the bindings of the sample site overlay,
// give nobody the power to act as another user: no impersonate, escalate or
// bind, and no binding to cluster-admin. A person who can act as another
// user can approve, or lift a guard, in that user's name, and the record of
// who decided names the wrong person.
package envtest_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// rbacFiles reads the Roles, ClusterRoles and bindings of the YAML files.
func rbacFiles(t *testing.T, files ...string) (roles []rbacv1.ClusterRole, bindings []rbacv1.RoleBinding) {
	t.Helper()
	for _, f := range files {
		for _, doc := range strings.Split(readFile(t, f), "\n---") {
			var meta metav1.TypeMeta
			require.NoError(t, yaml.Unmarshal([]byte(doc), &meta), f)
			switch meta.Kind {
			case "Role", "ClusterRole":
				var r rbacv1.ClusterRole // the same rules either way
				require.NoError(t, yaml.Unmarshal([]byte(doc), &r), f)
				roles = append(roles, r)
			case "RoleBinding", "ClusterRoleBinding":
				var b rbacv1.RoleBinding
				require.NoError(t, yaml.Unmarshal([]byte(doc), &b), f)
				bindings = append(bindings, b)
			}
		}
	}
	return roles, bindings
}

func shippedRBACFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "config", "rbac", "*.yaml"))
	require.NoError(t, err)
	files = slices.DeleteFunc(files, func(f string) bool { return filepath.Base(f) == "kustomization.yaml" })
	require.NotEmpty(t, files)
	return append(files, filepath.Join("..", "..", "config", "samples", "site-overlay", "bindings.yaml"),
		filepath.Join("..", "..", "config", "lab", "admins.yaml"))
}

func TestShippedRolesGrantNoPowerOverIdentities(t *testing.T) {
	roles, bindings := rbacFiles(t, shippedRBACFiles(t)...)
	require.NotEmpty(t, roles)
	for _, r := range roles {
		for _, rule := range r.Rules {
			for _, v := range rule.Verbs {
				assert.NotContains(t, []string{"*", "impersonate", "escalate", "bind"}, v, "role %s", r.Name)
			}
			assert.NotContains(t, rule.Resources, "*", "role %s", r.Name)
			assert.NotContains(t, rule.APIGroups, "*", "role %s", r.Name)
			assert.NotContains(t, rule.APIGroups, rbacv1.GroupName, "role %s changes RBAC", r.Name)
			assert.NotContains(t, rule.Resources, "serviceaccounts/token", "role %s mints tokens", r.Name)
		}
	}
	for _, b := range bindings {
		assert.NotContains(t, []string{"cluster-admin", "admin", "edit"}, b.RoleRef.Name,
			"binding %s gives a built-in role: it reads every Secret, and cluster-admin impersonates", b.Name)
	}
}

// pk-metal-admin covers each kind of the operator: a new CRD that it misses
// would send a site back to cluster-admin.
func TestAdminRoleCoversEachKind(t *testing.T) {
	roles, _ := rbacFiles(t, filepath.Join("..", "..", "config", "rbac", "admin_role.yaml"))
	granted := map[string]bool{}
	for _, r := range roles {
		if r.Name != "pk-metal-admin" {
			continue
		}
		for _, rule := range r.Rules {
			for _, g := range rule.APIGroups {
				for _, res := range rule.Resources {
					granted[g+"/"+res] = true
				}
			}
		}
	}
	crds, err := filepath.Glob(filepath.Join("..", "..", "config", "crd", "*_*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, crds)
	for _, f := range crds {
		group, plural, _ := strings.Cut(strings.TrimSuffix(filepath.Base(f), ".yaml"), "_")
		assert.True(t, granted[group+"/"+plural], "pk-metal-admin does not grant %s/%s", group, plural)
	}
}

// What the API server answers for a member of the admin group that the
// sample site overlay binds to pk-metal-admin.
func TestAdminRoleGrantsTheWorkNotTheIdentities(t *testing.T) {
	st := startEnv(t)
	cl := st.cl
	ctx := t.Context()
	ensureNamespace(t, ctx, cl, "pk-metal")
	ensureNamespace(t, ctx, cl, "team")
	roles, bindings := rbacFiles(t, filepath.Join("..", "..", "config", "rbac", "admin_role.yaml"),
		filepath.Join("..", "..", "config", "samples", "site-overlay", "bindings.yaml"))
	for _, r := range roles {
		if r.Namespace == "" {
			require.NoError(t, cl.Create(ctx, r.DeepCopy()))
			continue
		}
		require.NoError(t, cl.Create(ctx, &rbacv1.Role{ObjectMeta: r.ObjectMeta, Rules: r.Rules}))
	}
	for _, b := range bindings {
		if b.Name != "pk-metal-admin" {
			continue
		}
		if b.Namespace == "" {
			require.NoError(t, cl.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: b.ObjectMeta, RoleRef: b.RoleRef, Subjects: b.Subjects}))
			continue
		}
		require.NoError(t, cl.Create(ctx, b.DeepCopy()))
	}

	may := func(attrs authorizationv1.ResourceAttributes) bool {
		sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: "dana", Groups: []string{"pk-metal:admins", "system:authenticated"}, ResourceAttributes: &attrs}}
		require.NoError(t, cl.Create(ctx, sar))
		return sar.Status.Allowed
	}
	for _, tc := range []struct {
		what  string
		attrs authorizationv1.ResourceAttributes
		want  bool
	}{
		{"make a claim", authorizationv1.ResourceAttributes{Namespace: "team", Verb: "create", Group: "metal.platform-kit.org", Resource: "hostclaims"}, true},
		{"move a claim", authorizationv1.ResourceAttributes{Namespace: "team", Verb: "patch", Group: "metal.platform-kit.org", Resource: "hostclaims"}, true},
		{"change a cluster", authorizationv1.ResourceAttributes{Verb: "patch", Group: "os.platform-kit.org", Resource: "talosclusters"}, true},
		{"approve an operation", authorizationv1.ResourceAttributes{Namespace: "pk-metal", Verb: "patch", Group: "metal.platform-kit.org", Resource: "hostoperations"}, true},
		{"read a BMC login", authorizationv1.ResourceAttributes{Namespace: "pk-metal", Verb: "get", Resource: "secrets"}, true},
		{"change the site settings", authorizationv1.ResourceAttributes{Namespace: "pk-metal", Verb: "update", Resource: "configmaps"}, true},
		{"read the manager log", authorizationv1.ResourceAttributes{Namespace: "pk-metal", Verb: "get", Resource: "pods", Subresource: "log"}, true},

		{"act as another user", authorizationv1.ResourceAttributes{Verb: "impersonate", Resource: "users"}, false},
		{"act in another group", authorizationv1.ResourceAttributes{Verb: "impersonate", Resource: "groups"}, false},
		{"mint the operator's token", authorizationv1.ResourceAttributes{Namespace: "pk-metal", Verb: "create", Resource: "serviceaccounts", Subresource: "token"}, false},
		{"bind a role", authorizationv1.ResourceAttributes{Verb: "create", Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings"}, false},
		{"write what the operator found", authorizationv1.ResourceAttributes{Namespace: "pk-metal", Verb: "update", Group: "metal.platform-kit.org", Resource: "hostoperations", Subresource: "status"}, false},
		{"read Secrets elsewhere", authorizationv1.ResourceAttributes{Namespace: "kube-system", Verb: "get", Resource: "secrets"}, false},
		{"change the admission policies", authorizationv1.ResourceAttributes{Verb: "delete", Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicies"}, false},
	} {
		assert.Equal(t, tc.want, may(tc.attrs), tc.what)
	}
}
