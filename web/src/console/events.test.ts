import { describe, expect, it } from 'vitest';
import type { AttributedEvent } from '../api/hooks';
import { describeEvent } from './events';

function event(over: Partial<AttributedEvent> = {}): AttributedEvent {
  return {
    id: 1,
    type: 'transition',
    from_state: null,
    to_state: null,
    actor: 'system',
    payload: {},
    created_at: '2026-08-21T09:00:00Z',
    ...over,
  };
}

describe('describeEvent', () => {
  it('renders a transition using the badge vocabulary, not the raw states', () => {
    // A feed and a table must not disagree about what a state is called.
    expect(describeEvent(event({ from_state: 'provisioning', to_state: 'running' }))).toEqual({
      text: 'Pending → Running',
      tone: 'ok',
    });
  });

  it('marks a transition into a busy state as pending', () => {
    expect(describeEvent(event({ from_state: 'running', to_state: 'stopping' })).tone).toBe(
      'pending',
    );
  });

  it('marks a transition to stopped as muted rather than a success', () => {
    expect(describeEvent(event({ from_state: 'running', to_state: 'stopped' })).tone).toBe('muted');
  });

  it('includes the reason on an error transition', () => {
    const e = event({
      type: 'error',
      from_state: 'provisioning',
      to_state: 'error',
      payload: { reason: 'no space left on device' },
    });
    expect(describeEvent(e)).toEqual({
      text: 'Pending → Error: no space left on device',
      tone: 'error',
    });
  });

  it('handles an error with no reason', () => {
    expect(describeEvent(event({ type: 'error' })).text).toBe('Error');
    expect(describeEvent(event({ type: 'error' })).tone).toBe('error');
  });

  it('reports a successful clone and a failed one differently', () => {
    expect(describeEvent(event({ type: 'git.clone', payload: { ok: true } }))).toEqual({
      text: 'Repository cloned',
      tone: 'ok',
    });
    expect(
      describeEvent(event({ type: 'git.clone', payload: { ok: false, detail: 'auth failed' } })),
    ).toEqual({ text: 'Clone failed: auth failed', tone: 'error' });
  });

  it('handles a failed clone with no detail', () => {
    expect(describeEvent(event({ type: 'git.clone', payload: { ok: false } })).text).toBe(
      'Clone failed',
    );
  });

  it('reports a push the same way', () => {
    expect(describeEvent(event({ type: 'git.push', payload: { ok: true } })).text).toBe(
      'Pushed to remote',
    );
  });

  it('names the PR number and project', () => {
    const e = event({ type: 'git.pr', payload: { number: 42, project: 'proteos' } });
    expect(describeEvent(e)).toEqual({ text: 'Opened pull request #42 in proteos', tone: 'ok' });
  });

  it('survives a git.pr with a missing number', () => {
    // The payload is untyped; a missing field must not render "#undefined".
    const e = event({ type: 'git.pr', payload: { project: 'proteos' } });
    expect(describeEvent(e).text).toBe('Opened pull request in proteos');
  });

  it('ignores payload fields of the wrong type', () => {
    const e = event({ type: 'error', payload: { reason: { nested: true } } });
    expect(describeEvent(e).text).toBe('Error');
  });

  it('tolerates a missing payload entirely', () => {
    const e = {
      ...event({ type: 'info' }),
      payload: undefined as unknown as Record<string, never>,
    };
    expect(() => describeEvent(e)).not.toThrow();
  });

  it('falls back to the raw type for an unknown event with no states', () => {
    // A future server-side event type should produce a row, not a blank line.
    expect(describeEvent(event({ type: 'quota.exceeded' as AttributedEvent['type'] }))).toEqual({
      text: 'quota.exceeded',
      tone: 'muted',
    });
  });

  it('prefers a state pair over the raw type for an unknown event', () => {
    const e = event({
      type: 'quota.exceeded' as AttributedEvent['type'],
      from_state: 'running',
      to_state: 'stopped',
    });
    expect(describeEvent(e).text).toBe('Running → Stopped');
  });
});
