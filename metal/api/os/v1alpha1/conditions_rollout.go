package v1alpha1

// Condition types and reasons of day-2 changes: config drift, version skew,
// the secrets bundle guard, and rollouts.
const (
	// ConditionConfigInSync on a TalosNode: a dry-run apply of the rendered
	// config against the machine reports no change. False with reason
	// ConfigDrift says what differs and whether it needs a reboot.
	ConditionConfigInSync = "ConfigInSync"

	// ConditionVersionCurrent on a TalosNode: the machine runs the Talos
	// version the cluster asks for. False with reason VersionSkew while it
	// runs another one; the config is then rendered for the version it runs.
	ConditionVersionCurrent = "VersionCurrent"

	// ConditionVersionsSupported on a TalosCluster: Talos supports the
	// Kubernetes version the spec asks for. False with reason
	// UnsupportedVersionPair otherwise, and nothing renders.
	ConditionVersionsSupported = "VersionsSupported"

	// ConditionVersionsCurrent on a TalosCluster: every machine runs the
	// Talos version and the Kubernetes API runs the Kubernetes version the
	// spec asks for.
	ConditionVersionsCurrent = "VersionsCurrent"

	// ConditionSecretsBundleHeld on a TalosCluster: True while the secrets
	// bundle of the spec is not the bundle that the machines run. The operator
	// then continues to use the bundle that they run (reason BundleChanged).
	ConditionSecretsBundleHeld = "SecretsBundleHeld"

	// ConditionPlanned: on a TalosRollout, the operator made the plan for
	// all nodes. False means that it cannot plan a step yet.
	ConditionPlanned = "Planned"
	// ConditionApproved: on a TalosRollout, a person approved the plan, and
	// the approval names the plan as it is now.
	ConditionApproved = "Approved"
	// ConditionProgressing: on a TalosRollout, the rollout changes a node,
	// waits for health, or stays in a pause. False means that it did not
	// start or that it ended.
	ConditionProgressing = "Progressing"
	// ConditionPaused: on a TalosRollout, the rollout stays between two
	// nodes, after the first node or because a person paused it.
	ConditionPaused = "Paused"
)

// Reasons on a TalosNode's ConfigInSync and VersionCurrent conditions.
const (
	// ReasonInSync on ConfigInSync=True: the machine runs the rendered config.
	ReasonInSync = "InSync"
	// ReasonConfigDrift on ConfigInSync=False: a dry-run apply of the
	// rendered config reports a change. The message says what differs and
	// whether it needs a reboot.
	ReasonConfigDrift = "ConfigDrift"
	// ReasonDriftUnknown on ConfigInSync=Unknown: the dry-run apply failed.
	ReasonDriftUnknown = "DriftUnknown"
	// ReasonVersionCurrent on VersionCurrent=True: the machine runs the
	// Talos version that the cluster asks for. On a cluster's
	// VersionsCurrent=True, every machine and the Kubernetes API do.
	ReasonVersionCurrent = "VersionCurrent"
	// ReasonVersionSkew on VersionCurrent=False: the machine runs another
	// Talos version. The operator renders the config for the version that it
	// runs.
	ReasonVersionSkew = "VersionSkew"
)

// Reasons on a TalosCluster's VersionsSupported and VersionsCurrent
// conditions.
const (
	// ReasonUnsupportedVersion on VersionsSupported=False (and on a node's
	// Rendered=False): Talos does not support the Kubernetes version that
	// the spec asks for. Nothing renders.
	ReasonUnsupportedVersion = "UnsupportedVersionPair"
	// ReasonVersionsSupported on VersionsSupported=True: Talos supports the
	// Kubernetes version that the spec asks for.
	ReasonVersionsSupported = "VersionsSupported"
	// ReasonVersionsBehind on VersionsCurrent=False: a machine or the
	// Kubernetes API runs another version than the spec asks for. The
	// operator proposes an upgrade rollout.
	ReasonVersionsBehind = "VersionsBehind"
)

// Reasons on a TalosCluster's SecretsBundleHeld condition.
const (
	// ReasonBundleChanged on SecretsBundleHeld=True: the spec names another
	// secrets bundle than the machines run. The operator continues to use
	// the bundle that they run.
	ReasonBundleChanged = "BundleChanged"
	// ReasonBundleApplied on SecretsBundleHeld=False: the machines run the
	// bundle that the spec names.
	ReasonBundleApplied = "BundleApplied"
)

// Reasons on a TalosRollout's conditions.
const (
	// ReasonPlanReady on Planned=True: the operator planned a step for each
	// node that needs the change, or found no such node.
	ReasonPlanReady = "PlanReady"
	// ReasonPlanBlocked on Planned=False: the operator cannot plan a step
	// yet. The message says which step and why.
	ReasonPlanBlocked = "PlanBlocked"
	// ReasonNothingToDo on Complete=True: no node needs the change.
	ReasonNothingToDo = "NothingToDo"

	// ReasonAwaitingApproval on Approved=False: nobody approved the plan.
	ReasonAwaitingApproval = "AwaitingApproval"
	// ReasonPlanApproved on Approved=True.
	ReasonPlanApproved = "PlanApproved"
	// ReasonPlanChanged on Approved=False, Progressing=False and
	// Complete=False: the approval names a planHash the plan no longer has,
	// or a person not in spec.requestedBy changed the plan. Nothing runs;
	// approve a new rollout.
	ReasonPlanChanged = "PlanChanged"

	// ReasonNodeRunning on Progressing=True: a step runs on a node.
	ReasonNodeRunning = "NodeRunning"
	// ReasonWaitingForHealth on Progressing=True: the next step waits for
	// the health gate, or for another rollout of the cluster. On
	// Progressing=Unknown, the cluster does not exist.
	ReasonWaitingForHealth = "WaitingForHealth"
	// ReasonRolloutFailed on Progressing=False and Complete=False: a step
	// failed, and no other step starts. Progressing=True has this reason
	// until the last step that runs ends.
	ReasonRolloutFailed = "RolloutFailed"
	// ReasonRolloutAborted on Progressing=False and Complete=False: a person
	// aborted the rollout, and no other step starts. Progressing=True has
	// this reason until the last step that runs ends.
	ReasonRolloutAborted = "Aborted"
	// ReasonRolloutSucceeded on Progressing=False and Complete=True: each
	// step succeeded.
	ReasonRolloutSucceeded = "Succeeded"
	// ReasonCanaryPause on Paused=True and Progressing=True: the operator
	// changed the first node. The rollout waits until a person resumes it.
	ReasonCanaryPause = "PausedAfterFirstNode"
	// ReasonPausedByPerson on Paused=True and Progressing=True: spec.paused
	// is set.
	ReasonPausedByPerson = "PausedByPerson"
	// ReasonNotPaused on Paused=False.
	ReasonNotPaused = "NotPaused"
	// ReasonRolloutNotStarted on Progressing=False: nobody approved the
	// plan yet.
	ReasonRolloutNotStarted = "NotStarted"
)
