import { describe, expect, it } from 'vitest';
import { formatAge, gib } from './format';

const SEC = 1000;
const MIN = 60 * SEC;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;

// The admin console uses this to spot stale machines at a glance, so the unit
// BOUNDARIES are what matter: an age that rounds into the wrong unit misreports
// how long something has been sitting idle.
describe('formatAge', () => {
  it('collapses anything under a minute to "just now"', () => {
    expect(formatAge(0)).toBe('just now');
    expect(formatAge(59 * SEC)).toBe('just now');
  });

  it('switches to minutes at exactly one minute', () => {
    expect(formatAge(MIN)).toBe('1m ago');
    expect(formatAge(59 * MIN + 59 * SEC)).toBe('59m ago');
  });

  it('switches to hours at exactly one hour', () => {
    expect(formatAge(HOUR)).toBe('1h ago');
    expect(formatAge(23 * HOUR + 59 * MIN)).toBe('23h ago');
  });

  it('switches to days at exactly one day and does not cap', () => {
    expect(formatAge(DAY)).toBe('1d ago');
    expect(formatAge(400 * DAY)).toBe('400d ago');
  });

  it('truncates rather than rounds, so an age never reads as older than it is', () => {
    expect(formatAge(119 * MIN)).toBe('1h ago'); // 1h59m, not "2h"
  });

  // The server stamps these timestamps; the browser subtracts them from its own
  // clock. A browser running slightly behind therefore yields a negative delta,
  // which must not surface as "-3m ago".
  it('reads clock skew as "just now" rather than a negative age', () => {
    expect(formatAge(-5 * MIN)).toBe('just now');
  });
});

describe('gib', () => {
  it('renders whole gibibytes without a decimal', () => {
    expect(gib(2048)).toBe('2 GiB');
    expect(gib(1024)).toBe('1 GiB');
  });

  it('renders a fraction to one decimal', () => {
    expect(gib(1536)).toBe('1.5 GiB');
  });

  it('renders an absent size as an em dash', () => {
    // disk_mib is null until a machine has a disk; "undefined GiB" is worse.
    expect(gib(undefined)).toBe('—');
  });
});
