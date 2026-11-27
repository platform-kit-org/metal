package controller

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The pure half of the convergence lane: profile selection, the layered merge,
// schema validation, and plan hashing. Everything here is a function of its
// inputs — the reconciler supplies the cluster and BMC observations.

// selectorMatches evaluates a profile's match against a host's labels. An
// empty selector matches everything (the k8s convention — a fleet-wide
// profile); a malformed selector matches nothing and surfaces as an error.
func selectorMatches(sel metav1.LabelSelector, lbls map[string]string) (bool, error) {
	s, err := metav1.LabelSelectorAsSelector(&sel)
	if err != nil {
		return false, err
	}
	return s.Matches(labels.Set(lbls)), nil
}

// layered is one profile's value for one key, for mergeLayered.
type layered[V any] struct {
	key, profile string
	layer        int32
	value        V
}

// mergeLayered is the merge rule of every profile kind. It takes the entries
// in (layer, profile name) order, and a later layer overrides the value of a
// key. Two profiles on the same layer that disagree on a key are a conflict,
// and the first of them keeps the key. won holds the winner of each key;
// conflicts come back sorted.
func mergeLayered[V any](in []layered[V], equal func(a, b V) bool) (won map[string]layered[V], conflicts []string) {
	sorted := slices.Clone(in)
	slices.SortFunc(sorted, func(a, b layered[V]) int {
		return cmp.Or(cmp.Compare(a.layer, b.layer), strings.Compare(a.profile, b.profile))
	})
	won = map[string]layered[V]{}
	for _, e := range sorted {
		if prev, ok := won[e.key]; ok && prev.layer == e.layer && !equal(prev.value, e.value) {
			conflicts = append(conflicts, fmt.Sprintf("%s: profiles %s and %s disagree on layer %d", e.key, prev.profile, e.profile, e.layer))
			continue
		}
		won[e.key] = e
	}
	slices.Sort(conflicts)
	return won, conflicts
}

// mergeBios renders matched profiles into one desired attribute map, key by
// key (mergeLayered). Conflicts hold the whole materialization, so the
// caller must not apply attrs when any are returned. Provenance names the
// winning profile per key.
func mergeBios(profiles []metalv1alpha1.BiosProfile) (attrs map[string]apiextensionsv1.JSON, provenance map[string]string, conflicts []string) {
	var in []layered[apiextensionsv1.JSON]
	for _, p := range profiles {
		for k, v := range p.Spec.Attributes {
			in = append(in, layered[apiextensionsv1.JSON]{key: k, profile: p.Name, layer: p.Spec.Layer, value: v})
		}
	}
	won, conflicts := mergeLayered(in, func(a, b apiextensionsv1.JSON) bool { return jsonEqual(jsonAny(a), jsonAny(b)) })
	attrs, provenance = map[string]apiextensionsv1.JSON{}, map[string]string{}
	for k, w := range won {
		attrs[k], provenance[k] = w.value, w.profile
	}
	return attrs, provenance, conflicts
}

// mergeFirmware resolves matched profiles to one firmware target, which a
// later layer overrides whole, not per field.
func mergeFirmware(profiles []metalv1alpha1.FirmwareProfile) (target metalv1alpha1.FirmwareTarget, conflict string) {
	var in []layered[metalv1alpha1.FirmwareTarget]
	for _, p := range profiles {
		// A system-firmware-only profile says nothing about the manager.
		if p.Spec.Manager != nil {
			in = append(in, layered[metalv1alpha1.FirmwareTarget]{key: "manager", profile: p.Name, layer: p.Spec.Layer, value: *p.Spec.Manager})
		}
	}
	return mergeOne(in, func(a, b metalv1alpha1.FirmwareTarget) bool { return a == b })
}

// mergeOne is mergeLayered for the entries of one key: the winner, or the
// first conflict.
func mergeOne[V any](in []layered[V], equal func(a, b V) bool) (v V, conflict string) {
	won, conflicts := mergeLayered(in, equal)
	if len(conflicts) > 0 {
		return v, conflicts[0]
	}
	for _, w := range won {
		v = w.value
	}
	return v, ""
}

// validateBios checks desired values against the discovered attribute
// registry: unknown attributes would be rejected at PATCH, out-of-enum values
// silently dropped at apply — both must fail loudly here instead, before
// anything is proposed. A nil schema (endpoint serves no registry) validates
// nothing: absence is data, convergence then proves itself only by the
// verify-after-reboot read.
func validateBios(attrs map[string]apiextensionsv1.JSON, schema []bmc.BiosAttribute) []string {
	if schema == nil {
		return nil
	}
	byName := map[string]bmc.BiosAttribute{}
	for _, a := range schema {
		byName[a.Name] = a
	}
	var violations []string
	for k, j := range attrs {
		a, ok := byName[k]
		if !ok {
			violations = append(violations, k+": not in the attribute registry")
			continue
		}
		v := jsonAny(j)
		switch a.Type {
		case "Enumeration":
			s, ok := v.(string)
			if !ok || !slices.Contains(a.AllowedValues, s) {
				violations = append(violations,
					fmt.Sprintf("%s: %v not among allowed values %v", k, v, a.AllowedValues))
			}
		case "Boolean":
			if _, ok := v.(bool); !ok {
				violations = append(violations, fmt.Sprintf("%s: %v is not a boolean", k, v))
			}
		case "Integer":
			f, ok := v.(float64)
			switch {
			case !ok || math.Trunc(f) != f:
				violations = append(violations, fmt.Sprintf("%s: %v is not an integer", k, v))
			case math.Abs(f) > maxExactInteger:
				// Values travel as float64 (the seam's JSON typing). From 2^53 on
				// the decode rounds, so the operator would stage, and compare
				// against, a number nobody wrote.
				violations = append(violations,
					fmt.Sprintf("%s: %s is beyond ±(2^53-1), the largest integer staged exactly", k, j.Raw))
			}
		case "String":
			if _, ok := v.(string); !ok {
				violations = append(violations, fmt.Sprintf("%s: %v is not a string", k, v))
			}
		}
	}
	sort.Strings(violations)
	return violations
}

// maxExactInteger is 2^53-1, the largest integer that a float64 holds
// exactly with no other integer rounding to it (2^53+1 decodes to 2^53).
const maxExactInteger = 1<<53 - 1

// planHash hashes a desired state canonically (JSON with sorted map keys) —
// the identity an operation proposal is bound to.
func planHash(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// Desired states are decoded JSON values and API structs — both always
		// marshal; a failure here is a programming error worth a loud hash.
		return "sha256:unhashable"
	}
	return digest.FromBytes(raw).String()
}

// hash8 is the operation-name suffix for a plan hash.
func hash8(planHash string) string {
	return digest.Digest(planHash).Encoded()[:8]
}

// jsonAny decodes an arbitrary-JSON API value to its Go JSON type (string,
// bool, float64) — the same typing the seam serves, so the two compare.
func jsonAny(j apiextensionsv1.JSON) any {
	var v any
	_ = json.Unmarshal(j.Raw, &v)
	return v
}

// jsonOf re-encodes a seam value for storage in an API JSON field.
func jsonOf(v any) apiextensionsv1.JSON {
	raw, _ := json.Marshal(v)
	return apiextensionsv1.JSON{Raw: raw}
}

// jsonEqual compares two decoded JSON values.
func jsonEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

// decodeAttrs maps an API attribute map to seam typing.
func decodeAttrs(attrs map[string]apiextensionsv1.JSON) map[string]any {
	out := make(map[string]any, len(attrs))
	for k, v := range attrs {
		out[k] = jsonAny(v)
	}
	return out
}

// subsetJSON projects a machine-served map onto the managed keys, re-encoded
// for status — the observed/pending caches carry only what a profile manages.
func subsetJSON(state map[string]any, desired map[string]any) map[string]apiextensionsv1.JSON {
	out := map[string]apiextensionsv1.JSON{}
	for k := range desired {
		if v, ok := state[k]; ok {
			out[k] = jsonOf(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
