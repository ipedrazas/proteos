import { describe, expect, it } from 'vitest';
import { ApiError, SessionExpiredError } from '../api/client';
import { describeApiError } from './errors';

describe('describeApiError', () => {
  it('maps a known code to its own sentence', () => {
    expect(describeApiError(new ApiError(409, 'machine_limit'), 'x')).toBe(
      'You have reached your machine limit.',
    );
  });

  it('prefers a known code over the server detail', () => {
    // The mapped sentence is written for this UI; detail is generic prose.
    const err = new ApiError(409, 'machine_limit', 'limit is 5');
    expect(describeApiError(err, 'x')).toBe('You have reached your machine limit.');
  });

  it('falls back to the server detail for an unmapped code', () => {
    const err = new ApiError(400, 'invalid_resources', 'vcpus must be 1..8');
    expect(describeApiError(err, 'x')).toBe('vcpus must be 1..8');
  });

  it('falls back to the caller message for an unmapped code with no detail', () => {
    expect(describeApiError(new ApiError(500, 'boom'), 'Could not stop the machine.')).toBe(
      'Could not stop the machine.',
    );
  });

  it('recognises an expired session', () => {
    expect(describeApiError(new SessionExpiredError(), 'x')).toBe(
      'Your session expired. Sign in again.',
    );
  });

  it('falls back for a non-API error', () => {
    expect(describeApiError(new TypeError('network'), 'Something went wrong.')).toBe(
      'Something went wrong.',
    );
  });
});
