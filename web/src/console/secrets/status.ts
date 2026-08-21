// status turns a credential's timestamps into something a reader can act on.
//
// It exists because "expires_at: 2026-09-14T10:00:00Z" is not an answer to the
// question people actually have, which is "is this about to break something?".
// A token that expires next week and one that expired last month look almost
// identical as ISO strings and are completely different problems.
//
// `now` is a parameter rather than read from the clock, so the boundaries are
// testable rather than something you have to wait a week to observe.

import type { ProfileItem } from '../../api/client';

export type ExpiryState = 'never' | 'active' | 'soon' | 'expired' | 'unknown';

export interface Expiry {
  state: ExpiryState;
  label: string;
}

// A week: long enough to notice before something breaks, short enough that the
// warning still means something when it appears.
export const EXPIRING_SOON_MS = 7 * 24 * 60 * 60 * 1000;

export function describeExpiry(expiresAt: string | undefined | null, now: number): Expiry {
  if (!expiresAt) return { state: 'never', label: 'Never expires' };

  const at = Date.parse(expiresAt);
  // The API sends RFC3339 or omits the field, so this is a server bug rather
  // than a user-visible state. Say "unknown" instead of guessing "never" —
  // claiming a credential is permanent when we cannot tell is the worse error.
  if (Number.isNaN(at)) return { state: 'unknown', label: 'Unknown expiry' };

  const remaining = at - now;
  if (remaining <= 0) return { state: 'expired', label: 'Expired' };
  if (remaining <= EXPIRING_SOON_MS) {
    const days = Math.ceil(remaining / (24 * 60 * 60 * 1000));
    return { state: 'soon', label: days === 1 ? 'Expires in 1 day' : `Expires in ${days} days` };
  }
  return { state: 'active', label: `Expires ${new Date(at).toLocaleDateString()}` };
}

export type ItemState = 'connected' | 'attention' | 'absent';

export interface ItemStatus {
  state: ItemState;
  label: string;
}

// describeProfileItem collapses a portable profile item's three independent
// flags into the one thing the row should say. Order matters: an item that is
// connected AND needs reconnecting is a problem, not a success, so the warning
// wins over the happy state.
export function describeProfileItem(item: ProfileItem | undefined, now: number): ItemStatus {
  if (!item || !item.connected) return { state: 'absent', label: 'Not connected' };
  if (item.needs_reconnect) return { state: 'attention', label: 'Needs reconnecting' };

  const expiry = describeExpiry(item.expires_at, now);
  if (expiry.state === 'expired') return { state: 'attention', label: 'Expired' };
  if (expiry.state === 'soon') return { state: 'attention', label: expiry.label };
  return { state: 'connected', label: 'Connected' };
}
