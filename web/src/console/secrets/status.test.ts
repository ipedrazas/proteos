import { describe, expect, it } from 'vitest';
import type { ProfileItem } from '../../api/client';
import { describeExpiry, describeProfileItem, EXPIRING_SOON_MS } from './status';

const NOW = Date.parse('2026-08-21T09:00:00Z');
const iso = (offsetMs: number) => new Date(NOW + offsetMs).toISOString();
const DAY = 24 * 60 * 60 * 1000;

describe('describeExpiry', () => {
  it('reports no expiry when the field is absent', () => {
    expect(describeExpiry(undefined, NOW)).toEqual({ state: 'never', label: 'Never expires' });
    expect(describeExpiry(null, NOW).state).toBe('never');
    expect(describeExpiry('', NOW).state).toBe('never');
  });

  it('reports a past expiry as expired', () => {
    expect(describeExpiry(iso(-DAY), NOW)).toEqual({ state: 'expired', label: 'Expired' });
  });

  it('treats the exact expiry moment as expired', () => {
    expect(describeExpiry(iso(0), NOW).state).toBe('expired');
  });

  it('warns inside the last week, counting whole days', () => {
    expect(describeExpiry(iso(3 * DAY), NOW)).toEqual({
      state: 'soon',
      label: 'Expires in 3 days',
    });
  });

  it('says "1 day", not "1 days"', () => {
    expect(describeExpiry(iso(DAY - 1000), NOW).label).toBe('Expires in 1 day');
  });

  it('does not warn beyond a week', () => {
    expect(describeExpiry(iso(EXPIRING_SOON_MS + 1000), NOW).state).toBe('active');
  });

  it('warns exactly at the week boundary', () => {
    expect(describeExpiry(iso(EXPIRING_SOON_MS), NOW).state).toBe('soon');
  });

  it('says unknown rather than guessing "never" for an unparseable value', () => {
    // Claiming a credential is permanent when we cannot tell is the worse error.
    expect(describeExpiry('whenever', NOW)).toEqual({ state: 'unknown', label: 'Unknown expiry' });
  });
});

function item(over: Partial<ProfileItem> = {}): ProfileItem {
  return {
    key: 'claude-oauth',
    kind: 'oauth',
    target: 'claude',
    connected: true,
    needs_reconnect: false,
    created_at: iso(-30 * DAY),
    updated_at: iso(-30 * DAY),
    ...over,
  };
}

describe('describeProfileItem', () => {
  it('reports a missing item as not connected', () => {
    expect(describeProfileItem(undefined, NOW)).toEqual({
      state: 'absent',
      label: 'Not connected',
    });
  });

  it('reports a disconnected item as not connected', () => {
    expect(describeProfileItem(item({ connected: false }), NOW).state).toBe('absent');
  });

  it('reports a healthy item as connected', () => {
    expect(describeProfileItem(item(), NOW)).toEqual({ state: 'connected', label: 'Connected' });
  });

  it('lets needs_reconnect win over being connected', () => {
    // Connected AND stale is a problem, not a success.
    expect(describeProfileItem(item({ needs_reconnect: true }), NOW)).toEqual({
      state: 'attention',
      label: 'Needs reconnecting',
    });
  });

  it('flags a connected item whose token already expired', () => {
    expect(describeProfileItem(item({ expires_at: iso(-DAY) }), NOW)).toEqual({
      state: 'attention',
      label: 'Expired',
    });
  });

  it('flags a connected item expiring within the week', () => {
    expect(describeProfileItem(item({ expires_at: iso(2 * DAY) }), NOW)).toEqual({
      state: 'attention',
      label: 'Expires in 2 days',
    });
  });

  it('leaves a far-future expiry as plainly connected', () => {
    expect(describeProfileItem(item({ expires_at: iso(90 * DAY) }), NOW).state).toBe('connected');
  });

  it('prefers needs_reconnect over an expiry warning', () => {
    const s = describeProfileItem(item({ needs_reconnect: true, expires_at: iso(2 * DAY) }), NOW);
    expect(s.label).toBe('Needs reconnecting');
  });
});
