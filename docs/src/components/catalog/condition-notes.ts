// Text for the conditions that have no comment of their own in the Go source.
// Key: "<group>/<Condition>". ConditionTable uses it only when the generated
// description is empty or names a list of constants.
// Source: metal/internal/controller/talosrollout_controller.go.
export const conditionNotes: Record<string, string> = {
  'os.platform-kit.org/Planned': 'On a TalosRollout: the operator made the plan for all nodes. False means that it cannot plan a step yet.',
  'os.platform-kit.org/Approved': 'On a TalosRollout: a person approved the plan, and the approval names the plan as it is now.',
  'os.platform-kit.org/Progressing': 'On a TalosRollout: the rollout changes a node, waits for health, or stays in a pause. False means that it did not start or that it ended.',
  'os.platform-kit.org/Paused': 'On a TalosRollout: the rollout stays between two nodes, after the first node or because a person paused it.',
};
