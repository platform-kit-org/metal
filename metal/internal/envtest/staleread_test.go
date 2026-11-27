package envtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// A stale-read explorer for controllers that hand objects to each other.
//
// controller-runtime gives each kind one informer cache, shared by every
// controller of the manager. A cache is behind the API server, never ahead,
// and never goes back; two kinds are behind by different amounts. Writes
// carry the resourceVersion of the read (Update) or none (server-side apply).
// The TLA+ specification in spec/claimhost models exactly this; staleWorld
// runs the real reconcilers on it.
//
// A test drives a scripted path with fresh reads: each step either acts as
// the world outside the controllers under test (a person, the executor, a
// node), or runs a reconciler, which then writes to the API server. After
// each step, staleWorld records a snapshot of each recorded kind when it
// changed. Then it explores: it runs each target reconciler once on every
// combination of per-kind cache positions that a cache can show at that
// point, with every write sent as a dry run. A write that the API server
// accepts is checked against the invariants, and the API server is left as
// it was. The path then goes on.
//
// The positions a cache can show reach back to what the last reconcile pass
// on the path read: the caches are shared and never go back, so no later
// pass reads an older version. That floor is what makes a Host controller
// pass that reads an old Host and a new claim possible, and one that reads
// an old claim beside a Host its own release changed impossible.
//
// The explorer checks one stale pass away from the path, not every order of
// passes: the TLA+ model does that, on an abstraction of the same code.

// staleWorld is the API server as the caches of one manager see it.
type staleWorld struct {
	t      *testing.T
	live   client.Client // admin client: fresh reads, the record, the checks
	scheme *runtime.Scheme
	mgr    ctrl.Manager

	kinds   []schema.GroupVersionKind // recorded kinds, in a fixed order
	hist    map[schema.GroupVersionKind][][]client.Object
	floor   map[schema.GroupVersionKind]int
	indexes map[schema.GroupVersionKind]map[string]client.IndexerFunc

	// While exploring: the position each recorded kind's cache shows, and
	// the writes of the pass. Outside, pos is nil and reads are fresh.
	pos    map[schema.GroupVersionKind]int
	writes []staleWrite

	targets    []staleTarget
	invariants []staleInvariant
	explored   int
}

// staleWrite is one write that the API server accepted as a dry run.
type staleWrite struct {
	verb string // create, update, delete, patch, apply, status update, status patch, status apply
	obj  client.Object
}

// staleTarget is a reconcile that the explorer runs on stale caches.
type staleTarget struct {
	name string
	r    reconcile.Reconciler
	req  func() []reconcile.Request
}

// staleInvariant judges the writes of one stale pass. It returns what is
// wrong, or "".
type staleInvariant struct {
	name  string
	check func(ctx context.Context, p *stalePass) string
}

// stalePass is one explored reconcile pass.
type stalePass struct {
	w      *staleWorld
	target string
	req    reconcile.Request
	pos    map[schema.GroupVersionKind]int
	writes []staleWrite
}

// newStaleWorld makes a manager whose cache serves the recorded kinds from
// the snapshots, and whose client sends every write as a dry run while the
// explorer runs. Index functions come from the reconcilers' own
// SetupWithManager, through the manager's field indexer.
func newStaleWorld(t *testing.T, s *suite, kinds ...client.Object) *staleWorld {
	t.Helper()
	w := &staleWorld{
		t: t, live: s.cl, scheme: s.scheme,
		hist:    map[schema.GroupVersionKind][][]client.Object{},
		floor:   map[schema.GroupVersionKind]int{},
		indexes: map[schema.GroupVersionKind]map[string]client.IndexerFunc{},
	}
	for _, k := range kinds {
		gvk, err := apiutil.GVKForObject(k, s.scheme)
		require.NoError(t, err)
		w.kinds = append(w.kinds, gvk)
	}
	mgr, err := ctrl.NewManager(s.cfg, ctrl.Options{
		Scheme: s.scheme,
		NewCache: func(cfg *rest.Config, opts cache.Options) (cache.Cache, error) {
			inner, err := cache.New(cfg, opts)
			return &staleCache{Cache: inner, w: w}, err
		},
		NewClient: func(cfg *rest.Config, opts client.Options) (client.Client, error) {
			c, err := client.New(cfg, client.Options{Scheme: opts.Scheme, Mapper: opts.Mapper})
			if err != nil {
				return nil, err
			}
			return &staleClient{Client: c, dry: client.NewDryRunClient(c), w: w}, nil
		},
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err)
	w.mgr = mgr
	return w
}

// target adds a reconcile that the explorer runs after each step, for each
// request reqs returns then.
func (w *staleWorld) target(name string, r reconcile.Reconciler, reqs func() []reconcile.Request) {
	w.targets = append(w.targets, staleTarget{name: name, r: r, req: reqs})
}

// invariant adds a check of the writes of every explored pass.
func (w *staleWorld) invariant(name string, check func(ctx context.Context, p *stalePass) string) {
	w.invariants = append(w.invariants, staleInvariant{name: name, check: check})
}

// act runs one step of the world outside the controllers under test, with
// the admin client, then records and explores.
func (w *staleWorld) act(ctx context.Context, what string, fn func()) {
	w.t.Helper()
	fn()
	w.record(ctx)
	w.explore(ctx, what)
}

// reconcile runs one fresh pass of r on req: it reads what the API server
// holds and writes for real. The caches then hold at least this version of
// every kind, which raises the floor of the explorer.
func (w *staleWorld) reconcile(ctx context.Context, name string, r reconcile.Reconciler, req reconcile.Request) {
	w.t.Helper()
	w.record(ctx)
	for _, k := range w.kinds {
		w.floor[k] = len(w.hist[k]) - 1
	}
	_, err := r.Reconcile(ctx, req)
	if err != nil && !apierrors.IsConflict(err) {
		require.NoError(w.t, err, "%s %s", name, req)
	}
	w.record(ctx)
	w.explore(ctx, fmt.Sprintf("%s %s", name, req.NamespacedName))
}

// settle reconciles r on req until a pass writes nothing, at most 10 times.
func (w *staleWorld) settle(ctx context.Context, name string, r reconcile.Reconciler, req reconcile.Request) {
	w.t.Helper()
	for range 10 {
		before := w.heads()
		w.reconcile(ctx, name, r, req)
		if slices.Equal(before, w.heads()) {
			return
		}
	}
	w.t.Fatalf("%s %s did not settle in 10 passes", name, req)
}

func (w *staleWorld) heads() []int {
	out := make([]int, 0, len(w.kinds))
	for _, k := range w.kinds {
		out = append(out, len(w.hist[k]))
	}
	return out
}

// record appends a snapshot of each recorded kind that changed.
func (w *staleWorld) record(ctx context.Context) {
	w.t.Helper()
	for _, k := range w.kinds {
		objs := w.listLive(ctx, k)
		h := w.hist[k]
		if len(h) > 0 && sameVersions(h[len(h)-1], objs) {
			continue
		}
		w.hist[k] = append(h, objs)
	}
}

func sameVersions(a, b []client.Object) bool {
	return slices.EqualFunc(a, b, func(x, y client.Object) bool {
		return x.GetUID() == y.GetUID() && x.GetResourceVersion() == y.GetResourceVersion()
	})
}

func (w *staleWorld) listLive(ctx context.Context, gvk schema.GroupVersionKind) []client.Object {
	w.t.Helper()
	list := w.newList(gvk)
	require.NoError(w.t, w.live.List(ctx, list))
	items, err := meta.ExtractList(list)
	require.NoError(w.t, err)
	out := make([]client.Object, 0, len(items))
	for _, it := range items {
		out = append(out, it.(client.Object))
	}
	slices.SortFunc(out, func(a, b client.Object) int {
		return strings.Compare(a.GetNamespace()+"/"+a.GetName(), b.GetNamespace()+"/"+b.GetName())
	})
	return out
}

func (w *staleWorld) newList(gvk schema.GroupVersionKind) client.ObjectList {
	w.t.Helper()
	obj, err := w.scheme.New(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	require.NoError(w.t, err)
	return obj.(client.ObjectList)
}

// explore runs every target on every combination of cache positions from
// the floor to the head of each recorded kind, and checks the invariants.
func (w *staleWorld) explore(ctx context.Context, after string) {
	w.t.Helper()
	ranges := make([][2]int, len(w.kinds))
	for i, k := range w.kinds {
		ranges[i] = [2]int{w.floor[k], len(w.hist[k]) - 1}
	}
	var combos [][]int
	var walk func(i int, cur []int)
	walk = func(i int, cur []int) {
		if i == len(ranges) {
			combos = append(combos, slices.Clone(cur))
			return
		}
		for p := ranges[i][0]; p <= ranges[i][1]; p++ {
			walk(i+1, append(cur, p))
		}
	}
	walk(0, nil)
	require.LessOrEqual(w.t, len(combos), 4096, "after %s: too many cache states to explore; reconcile more often", after)

	for _, tg := range w.targets {
		for _, req := range tg.req() {
			for _, combo := range combos {
				w.pos = map[schema.GroupVersionKind]int{}
				for i, k := range w.kinds {
					w.pos[k] = combo[i]
				}
				w.writes = nil
				_, _ = tg.r.Reconcile(ctx, req) // a conflict or a missing object ends a pass; that is fine
				p := &stalePass{w: w, target: tg.name, req: req, pos: w.pos, writes: w.writes}
				w.pos, w.writes = nil, nil
				w.explored++
				for _, inv := range w.invariants {
					if msg := inv.check(ctx, p); msg != "" {
						w.t.Errorf("after %s: %s %s on stale caches (%s) violates %q: %s",
							after, tg.name, req.NamespacedName, p.describe(), inv.name, msg)
					}
				}
			}
		}
	}
}

// describe names the version each cache showed, as "kind i/n".
func (p *stalePass) describe() string {
	var parts []string
	for _, k := range p.w.kinds {
		n := len(p.w.hist[k]) - 1
		if p.pos[k] < n {
			parts = append(parts, fmt.Sprintf("%s %d writes behind", k.Kind, n-p.pos[k]))
		}
	}
	if len(parts) == 0 {
		return "all current"
	}
	return strings.Join(parts, ", ")
}

// read returns the version of obj's kind and key that the pass read from its
// cache, or false when the cache had none.
func (p *stalePass) read(obj client.Object) bool {
	gvk, err := apiutil.GVKForObject(obj, p.w.scheme)
	if err != nil {
		return false
	}
	for _, o := range p.w.hist[gvk][p.pos[gvk]] {
		if o.GetNamespace() == obj.GetNamespace() && o.GetName() == obj.GetName() {
			setObject(obj, o)
			return true
		}
	}
	return false
}

// current returns obj as the API server holds it after the earlier writes of
// the pass: a dry run stores nothing, so a later check of the same pass
// reads them from here.
func (p *stalePass) current(ctx context.Context, obj client.Object, before int) bool {
	for i := before - 1; i >= 0; i-- {
		wr := p.writes[i]
		if sameKind(p.w.scheme, wr.obj, obj) && wr.obj.GetNamespace() == obj.GetNamespace() && wr.obj.GetName() == obj.GetName() &&
			!strings.HasPrefix(wr.verb, "status") {
			if wr.verb == "delete" {
				return false
			}
			setObject(obj, wr.obj)
			return true
		}
	}
	return p.w.live.Get(ctx, client.ObjectKeyFromObject(obj), obj) == nil
}

func sameKind(s *runtime.Scheme, a, b client.Object) bool {
	ga, err1 := apiutil.GVKForObject(a, s)
	gb, err2 := apiutil.GVKForObject(b, s)
	return err1 == nil && err2 == nil && ga == gb
}

// setObject copies src into dst; both are the same typed kind.
func setObject(dst, src client.Object) {
	reflect.ValueOf(dst).Elem().Set(reflect.ValueOf(src.DeepCopyObject()).Elem())
}

// --- the cache ---------------------------------------------------------------

// staleCache serves reads of the recorded kinds from the snapshot at the
// explored position, and every other read from the API server. It records
// the index functions the reconcilers register, and filters by them itself:
// the API server cannot.
type staleCache struct {
	cache.Cache // only for the informer plumbing of SetupWithManager; never started
	w           *staleWorld
}

func (c *staleCache) IndexField(ctx context.Context, obj client.Object, field string, fn client.IndexerFunc) error {
	gvk, err := apiutil.GVKForObject(obj, c.w.scheme)
	if err != nil {
		return err
	}
	if c.w.indexes[gvk] == nil {
		c.w.indexes[gvk] = map[string]client.IndexerFunc{}
	}
	c.w.indexes[gvk][field] = fn
	return c.Cache.IndexField(ctx, obj, field, fn)
}

func (c *staleCache) snapshot(gvk schema.GroupVersionKind) ([]client.Object, bool) {
	if c.w.pos == nil {
		return nil, false
	}
	p, ok := c.w.pos[gvk]
	if !ok {
		return nil, false
	}
	return c.w.hist[gvk][p], true
}

func (c *staleCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	gvk, err := apiutil.GVKForObject(obj, c.w.scheme)
	if err != nil {
		return err
	}
	objs, ok := c.snapshot(gvk)
	if !ok {
		return c.w.live.Get(ctx, key, obj, opts...)
	}
	for _, o := range objs {
		if o.GetNamespace() == key.Namespace && o.GetName() == key.Name {
			setObject(obj, o)
			return nil
		}
	}
	return apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: strings.ToLower(gvk.Kind)}, key.Name)
}

func (c *staleCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	lgvk, err := apiutil.GVKForObject(list, c.w.scheme)
	if err != nil {
		return err
	}
	gvk := lgvk.GroupVersion().WithKind(strings.TrimSuffix(lgvk.Kind, "List"))
	lo := (&client.ListOptions{}).ApplyOptions(opts)
	objs, ok := c.snapshot(gvk)
	if !ok {
		// The API server filters by namespace and labels, not by an index.
		live := []client.ListOption{client.InNamespace(lo.Namespace)}
		if lo.LabelSelector != nil {
			live = append(live, client.MatchingLabelsSelector{Selector: lo.LabelSelector})
		}
		if err := c.w.live.List(ctx, list, live...); err != nil {
			return err
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return err
		}
		objs = nil
		for _, it := range items {
			objs = append(objs, it.(client.Object))
		}
	}
	var keep []runtime.Object
	for _, o := range objs {
		if lo.Namespace != "" && o.GetNamespace() != lo.Namespace {
			continue
		}
		if lo.LabelSelector != nil && !lo.LabelSelector.Matches(labels.Set(o.GetLabels())) {
			continue
		}
		if !c.matchesFields(gvk, o, lo) {
			continue
		}
		keep = append(keep, o.DeepCopyObject())
	}
	return meta.SetList(list, keep)
}

func (c *staleCache) matchesFields(gvk schema.GroupVersionKind, o client.Object, lo *client.ListOptions) bool {
	if lo.FieldSelector == nil {
		return true
	}
	for _, req := range lo.FieldSelector.Requirements() {
		fn := c.w.indexes[gvk][req.Field]
		if fn == nil {
			c.w.t.Fatalf("list of %s by field %s: no index registered", gvk.Kind, req.Field)
		}
		if !slices.Contains(fn(o), req.Value) {
			return false
		}
	}
	return true
}

// --- the client ----------------------------------------------------------------

// staleClient reads through the staleCache. On the scripted path it writes
// to the API server; while the explorer runs, every write is a dry run, and
// each one the API server accepts is recorded for the invariants.
type staleClient struct {
	client.Client
	dry client.Client
	w   *staleWorld
}

func (c *staleClient) exploring() bool { return c.w.pos != nil }

func (c *staleClient) note(verb string, obj client.Object) {
	c.w.writes = append(c.w.writes, staleWrite{verb: verb, obj: obj.DeepCopyObject().(client.Object)})
}

func (c *staleClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.w.mgr.GetCache().Get(ctx, key, obj, opts...)
}

func (c *staleClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.w.mgr.GetCache().List(ctx, list, opts...)
}

// writer is where a write goes: a dry run while the explorer runs, else the
// API server.
func (c *staleClient) writer() client.Client {
	if c.exploring() {
		return c.dry
	}
	return c.Client
}

// done records a write that the API server accepted while the explorer
// runs, and returns err.
func (c *staleClient) done(verb string, obj client.Object, err error) error {
	if err == nil && c.exploring() {
		c.note(verb, obj)
	}
	return err
}

// doneApply is done for an apply.
func (c *staleClient) doneApply(verb string, ac runtime.ApplyConfiguration, err error) error {
	if err == nil && c.exploring() {
		c.noteApply(verb, ac)
	}
	return err
}

func (c *staleClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.done("create", obj, c.writer().Create(ctx, obj, opts...))
}

func (c *staleClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.done("update", obj, c.writer().Update(ctx, obj, opts...))
}

func (c *staleClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return c.done("delete", obj, c.writer().Delete(ctx, obj, opts...))
}

func (c *staleClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return c.done("patch", obj, c.writer().Patch(ctx, obj, patch, opts...))
}

func (c *staleClient) Apply(ctx context.Context, ac runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	return c.doneApply("apply", ac, c.writer().Apply(ctx, ac, opts...))
}

// noteApply records an apply configuration as the typed object it sets.
func (c *staleClient) noteApply(verb string, ac runtime.ApplyConfiguration) {
	raw, err := json.Marshal(ac)
	require.NoError(c.w.t, err)
	var tm struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	require.NoError(c.w.t, json.Unmarshal(raw, &tm))
	obj, err := c.w.scheme.New(schema.FromAPIVersionAndKind(tm.APIVersion, tm.Kind))
	require.NoError(c.w.t, err)
	require.NoError(c.w.t, json.Unmarshal(raw, obj))
	c.note(verb, obj.(client.Object))
}

func (c *staleClient) Status() client.SubResourceWriter {
	return &staleStatus{c: c}
}

// SubResource writes other than status (eviction, scale) are not dry-run
// here: no reconciler under test makes one, and the explorer fails if it
// does.
func (c *staleClient) SubResource(sub string) client.SubResourceClient {
	if c.exploring() {
		c.w.t.Fatalf("an explored pass used the %s subresource, which the explorer does not dry-run", sub)
	}
	return c.Client.SubResource(sub)
}

type staleStatus struct{ c *staleClient }

func (s *staleStatus) Create(ctx context.Context, obj, sub client.Object, opts ...client.SubResourceCreateOption) error {
	return s.c.writer().Status().Create(ctx, obj, sub, opts...)
}

func (s *staleStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return s.c.done("status update", obj, s.c.writer().Status().Update(ctx, obj, opts...))
}

func (s *staleStatus) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return s.c.done("status patch", obj, s.c.writer().Status().Patch(ctx, obj, patch, opts...))
}

func (s *staleStatus) Apply(ctx context.Context, ac runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	return s.c.doneApply("status apply", ac, s.c.writer().Status().Apply(ctx, ac, opts...))
}
