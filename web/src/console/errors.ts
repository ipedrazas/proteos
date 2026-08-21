// errors turns a thrown API error into a sentence a user can act on.
//
// The control plane answers failures with a stable machine-readable `code` and
// an optional human `detail` (e.g. invalid_resources → "vcpus must be 1..8").
// Rendering the raw code puts the server's vocabulary on screen; rendering only
// a generic message throws away the one part that says what to do instead. This
// maps the codes the console can actually provoke and falls back to `detail`,
// which is already written for a reader, before giving up on a generic line.

import { ApiError, SessionExpiredError } from '../api/client';

const MESSAGES: Record<string, string> = {
  machine_limit: 'You have reached your machine limit.',
  unknown_template: 'That template is no longer available.',
  machine_not_running: 'That machine is not running.',
  no_machine: 'That machine no longer exists.',
  guest_unreachable: 'The machine is not responding.',
  session_export_failed: 'Could not export this machine’s coding-agent sessions.',
  rate_limited: 'Too many requests — wait a moment and try again.',
};

export function describeApiError(error: unknown, fallback: string): string {
  if (error instanceof SessionExpiredError) return 'Your session expired. Sign in again.';
  if (error instanceof ApiError) {
    return MESSAGES[error.code] ?? error.detail ?? fallback;
  }
  return fallback;
}
