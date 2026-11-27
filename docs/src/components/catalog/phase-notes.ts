// What each phase means and when the operator shows it. The names come from the
// generated phases.json; the rules come from metal/internal/phase/phase.go, which is a
// list of rules in order of precedence (the first rule that applies gives the phase).
// PhaseTable stops the build when a phase has no note here, or a note has no phase.
export interface PhaseNote {
  /** One sentence for the reader: what the phase tells you. */
  means: string;
  /** The rule in phase.go. Text in `backticks` shows as code. */
  rule: string;
  /** 1 = the rule that the operator examines first. */
  order: number;
  tone?: 'ok' | 'warn' | 'err' | 'accent' | 'soft';
}

export const phaseNotes: Record<string, Record<string, PhaseNote>> = {
  HostPhase: {
    Inspecting: { order: 7, means: 'The BMC answers. The operator reads the hardware and compares the serial number.', rule: 'No other rule applies.' },
    Available: { order: 6, tone: 'accent', means: 'The operator knows the machine, and no claim holds it.', rule: '`Inspected` and `Verified` are True.' },
    Assigned: { order: 5, means: 'A claim holds the machine. No work on the machine started.', rule: 'A claim holds the Host.' },
    Provisioning: { order: 4, means: 'An install runs on the machine, or its node does not answer now.', rule: 'A claim holds the Host, and `OSReady` is present.' },
    Provisioned: { order: 3, tone: 'ok', means: 'The node on the machine reports that it is ready.', rule: 'A claim holds the Host, and `OSReady` is True.' },
    Releasing: { order: 2, tone: 'warn', means: 'The claim is gone. The operator cleans the machine and frees it.', rule: 'The claim that holds the Host is in deletion, or it is gone and the Host still names it.' },
    Maintenance: { order: 1, tone: 'soft', means: 'A person took the machine out of the lifecycle for hardware work.', rule: 'The Host has the annotation `metal.platform-kit.org/maintenance`.' },
  },
  ClaimPhase: {
    Pending: { order: 3, means: 'The claim holds no Host. If `blockedOn` is empty, the operator waits for the machine to appear.', rule: '`Bound` is absent, or False with a reason that is not `AmbiguousSystem`.' },
    Bound: { order: 1, tone: 'ok', means: 'The claim holds one Host. If an install waits for a person, `blockedOn` gives the reason.', rule: '`Bound` is True.' },
    Ambiguous: { order: 2, tone: 'err', means: 'The BMC manages more than one machine, and the claim does not name one. The operator does not select one for you.', rule: '`Bound` is False with the reason `AmbiguousSystem`.' },
  },
  OperationPhase: {
    Proposed: { order: 6, means: 'The operation exists and did not start. With a `blockedOn` reason, it has an approval but waits, for example for a maintenance window.', rule: '`Approved` is absent. Or `Approved` is True and `Executed` has a reason that is not `Executing`.' },
    AwaitingApproval: { order: 4, tone: 'warn', means: 'The operation waits for a person to approve it.', rule: '`Approved` is present and not True.' },
    Running: { order: 5, tone: 'accent', means: 'The operator does the actions, one after the other.', rule: '`Approved` is True, and `Executed` is absent or has the reason `Executing`.' },
    Succeeded: { order: 1, tone: 'ok', means: 'All actions are complete. This phase is final.', rule: '`Executed` is True.' },
    Failed: { order: 3, tone: 'err', means: 'An action failed. The operator does not try it again. This phase is final.', rule: '`Executed` is False with a reason that is not `Aborted`.' },
    Aborted: { order: 2, tone: 'soft', means: 'A person stopped the operation. This phase is final.', rule: '`Executed` is False with the reason `Aborted`.' },
  },
  EndpointPhase: {
    Unreachable: { order: 1, tone: 'err', means: 'Nothing answered at the address, or the login failed.', rule: '`Reachable` is not True.' },
    Mismatch: { order: 2, tone: 'err', means: 'The BMC at this address is not the machine that the Asset describes. The operator does nothing to it.', rule: '`Verified` is False.' },
    Inventorying: { order: 3, means: 'The operator reads the facts of the BMC: vendor, model, systems, media slots and firmware.', rule: '`Inventoried` is not True.' },
    Verifying: { order: 4, means: 'The operator compares the MAC address and the serial number with the Asset.', rule: '`Verified` is not True.' },
    Configuring: { order: 5, tone: 'warn', means: 'An EndpointProfile applies, and the operator configures the BMC.', rule: '`Configured` is present and not True.' },
    Ready: { order: 6, tone: 'ok', means: 'The BMC answers, the identity agrees, and the configuration is complete.', rule: 'No other rule applies.' },
  },
};
