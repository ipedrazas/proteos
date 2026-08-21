// machineState maps a machine's server-side state onto what the console shows
// and what it lets you do. It is the module most likely to drift as states are
// added to the control plane, so it lives on its own and is unit-tested rather
// than being scattered through the table's JSX.
//
// The mapping deliberately collapses the API's eight states into four tones. A
// reader scanning a fleet wants to know "is it up, is it busy, is it off, is it
// broken" — the difference between `requested` and `provisioning` matters to
// the scheduler, not to that judgement. The precise state stays available as
// the badge's title attribute.

import type { MachineState } from '../api/client';

type StateTone = 'ok' | 'pending' | 'idle' | 'danger';

export interface StatePresentation {
  label: string;
  tone: StateTone;
  // busy marks a transition the control plane is still working through. Such a
  // machine takes no lifecycle action: the answer would be racing whatever the
  // server is already doing, and the SSE stream will settle it shortly.
  busy: boolean;
}

export function describeState(state: MachineState): StatePresentation {
  switch (state) {
    case 'running':
      return { label: 'Running', tone: 'ok', busy: false };
    case 'stopped':
      return { label: 'Stopped', tone: 'idle', busy: false };
    case 'error':
      return { label: 'Error', tone: 'danger', busy: false };
    case 'requested':
    case 'provisioning':
    case 'starting':
      return { label: 'Pending', tone: 'pending', busy: true };
    case 'stopping':
    case 'hibernating':
      return { label: 'Stopping', tone: 'pending', busy: true };
    default:
      // A state this build does not know about — the control plane is ahead of
      // the SPA. Show it verbatim and treat it as inert rather than guessing:
      // an unknown state is exactly when acting on a wrong assumption is worst.
      return { label: state, tone: 'idle', busy: false };
  }
}

// MachineActions is which lifecycle actions apply to one machine right now.
// Terminate is absent because it always applies — see canTerminate below.
export interface MachineActions {
  start: boolean;
  stop: boolean;
}

export function machineActions(state: MachineState): MachineActions {
  return {
    // Only a stopped machine starts. `error` is deliberately excluded: the
    // recovery path for a failed machine is to terminate and relaunch, not to
    // retry a start the control plane already gave up on.
    start: state === 'stopped',
    stop: state === 'running',
  };
}

// canTerminate is true for every state, including the busy ones — this is a
// function rather than a constant so the rule is stated once and the callers
// read the same way as they do for start/stop. Tearing down a machine that is
// stuck provisioning is a legitimate and fairly common thing to want, and the
// control plane owns the ordering of a destroy against an in-flight transition.
export function canTerminate(): boolean {
  return true;
}

// TerminalGate answers whether a machine can take a terminal connection right
// now, and what to say if not.
//
// This exists because the browser cannot tell the difference (TAV-185). The
// gateway refuses a non-running machine with 409 machine_not_running, but the
// WebSocket API hides every pre-upgrade HTTP status — so an ungated terminal on
// a stopped machine looks exactly like a flaky network: five silent reconnect
// attempts with backoff, then a generic "closed". The refusal is correct and
// the UI has to supply the explanation the protocol throws away.
export interface TerminalGate {
  ok: boolean;
  reason: string;
  // offerStart is true when the fix is simply to start the machine, so the
  // blocked panel can carry the action instead of sending the user elsewhere.
  offerStart: boolean;
}

export function terminalGate(state: MachineState): TerminalGate {
  if (state === 'running') return { ok: true, reason: '', offerStart: false };
  if (state === 'stopped') {
    return {
      ok: false,
      reason: 'This machine is stopped. Start it to open a terminal.',
      offerStart: true,
    };
  }
  if (state === 'error') {
    return {
      ok: false,
      reason: 'This machine is in an error state and cannot accept connections.',
      offerStart: false,
    };
  }
  // Every remaining state is a transition the control plane is working through.
  // Say which one, so the wait reads as progress rather than as a hang.
  return {
    ok: false,
    reason: `This machine is ${describeState(state).label.toLowerCase()}. The terminal opens once it is running.`,
    offerStart: false,
  };
}
