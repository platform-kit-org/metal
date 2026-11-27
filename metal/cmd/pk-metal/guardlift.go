package main

import (
	"strings"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// requestedBy names the caller on a request: the username for the annotation
// metal.platform-kit.org/requested-by and, when ack gives a reason, the guard
// lift metal.platform-kit.org/ack-etcd-risk as "<username>: <reason>", the
// only form admission accepts (config/policy/guard-lift.yaml).
//
// Where the site forbids self-approval, the requested-by admission policy
// wants the name on each request that makes the operator propose work, so
// that the approval policies can refuse an approval by the person who asked
// for the change. pk-metal sets it in both modes: the operator copies it into
// the audit trail.
func requestedBy(user, ack string) map[string]any {
	set := map[string]any{metalv1alpha1.AnnotationRequestedBy: user}
	if strings.TrimSpace(ack) != "" {
		set[metalv1alpha1.AnnotationAckEtcdRisk] = metalv1alpha1.GuardLiftValue(user, ack)
	}
	return set
}
