import { describe, expect, it } from 'vitest';
import type { MachineState } from '../api/client';
import { canTerminate, describeState, machineActions, terminalGate } from './machineState';

const ALL_STATES: MachineState[] = [
  'requested',
  'provisioning',
  'running',
  'starting',
  'stopping',
  'hibernating',
  'stopped',
  'error',
];

describe('describeState', () => {
  it('maps the settled states to their own tone', () => {
    expect(describeState('running')).toEqual({ label: 'Running', tone: 'ok', busy: false });
    expect(describeState('stopped')).toEqual({ label: 'Stopped', tone: 'idle', busy: false });
    expect(describeState('error')).toEqual({ label: 'Error', tone: 'danger', busy: false });
  });

  it('collapses the spin-up states into one Pending badge', () => {
    for (const s of ['requested', 'provisioning', 'starting'] as MachineState[]) {
      expect(describeState(s)).toEqual({ label: 'Pending', tone: 'pending', busy: true });
    }
  });

  it('collapses the wind-down states into one Stopping badge', () => {
    for (const s of ['stopping', 'hibernating'] as MachineState[]) {
      expect(describeState(s)).toEqual({ label: 'Stopping', tone: 'pending', busy: true });
    }
  });

  it('describes every state the API can return', () => {
    for (const s of ALL_STATES) expect(describeState(s).label).not.toBe('');
  });

  it('shows an unknown state verbatim and treats it as inert', () => {
    // The control plane may add a state before the SPA knows about it.
    const future = describeState('quiescing' as MachineState);
    expect(future).toEqual({ label: 'quiescing', tone: 'idle', busy: false });
  });

  it('marks exactly the transitional states busy', () => {
    const busy = ALL_STATES.filter((s) => describeState(s).busy);
    expect(busy).toEqual(['requested', 'provisioning', 'starting', 'stopping', 'hibernating']);
  });
});

describe('machineActions', () => {
  it('offers start only for a stopped machine', () => {
    const startable = ALL_STATES.filter((s) => machineActions(s).start);
    expect(startable).toEqual(['stopped']);
  });

  it('offers stop only for a running machine', () => {
    const stoppable = ALL_STATES.filter((s) => machineActions(s).stop);
    expect(stoppable).toEqual(['running']);
  });

  it('never offers start and stop at once', () => {
    for (const s of ALL_STATES) {
      const a = machineActions(s);
      expect(a.start && a.stop).toBe(false);
    }
  });

  it('offers no lifecycle action while a machine is busy', () => {
    for (const s of ALL_STATES.filter((x) => describeState(x).busy)) {
      expect(machineActions(s)).toEqual({ start: false, stop: false });
    }
  });

  it('does not offer to start a machine in error', () => {
    // Recovery is terminate-and-relaunch; the control plane already gave up.
    expect(machineActions('error').start).toBe(false);
  });
});

describe('canTerminate', () => {
  it('allows terminating in any state, including busy ones', () => {
    expect(canTerminate()).toBe(true);
  });
});

describe('terminalGate', () => {
  it('allows a running machine', () => {
    expect(terminalGate('running')).toEqual({ ok: true, reason: '', offerStart: false });
  });

  it('blocks every state that is not running', () => {
    for (const s of ALL_STATES.filter((x) => x !== 'running')) {
      expect(terminalGate(s).ok).toBe(false);
      expect(terminalGate(s).reason).not.toBe('');
    }
  });

  it('offers Start only for a stopped machine', () => {
    const offering = ALL_STATES.filter((s) => terminalGate(s).offerStart);
    expect(offering).toEqual(['stopped']);
  });

  it('names the transition it is waiting on', () => {
    // "Pending" reads as progress; a generic "cannot connect" reads as broken.
    expect(terminalGate('provisioning').reason).toContain('pending');
    expect(terminalGate('stopping').reason).toContain('stopping');
  });

  it('does not offer Start for a machine in error', () => {
    expect(terminalGate('error').offerStart).toBe(false);
    expect(terminalGate('error').reason).toContain('error state');
  });
});
