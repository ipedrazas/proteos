import { describe, expect, it } from 'vitest';
import { failures, runBulk } from './bulk';

// deferred makes a promise this test resolves by hand, so concurrency can be
// observed rather than timed.
function deferred<T = void>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe('runBulk', () => {
  it('returns one outcome per item, in input order', async () => {
    const out = await runBulk([1, 2, 3], async (n) => n * 2);
    expect(out.map((o) => o.item)).toEqual([1, 2, 3]);
    expect(out.every((o) => o.ok)).toBe(true);
  });

  it('keeps input order even when items finish out of order', async () => {
    const gates = [deferred(), deferred(), deferred()];
    const run = runBulk([0, 1, 2], (i) => gates[i].promise);
    // Finish backwards.
    gates[2].resolve();
    gates[1].resolve();
    gates[0].resolve();
    const out = await run;
    expect(out.map((o) => o.item)).toEqual([0, 1, 2]);
  });

  it('captures a failure instead of rejecting, and lets the rest finish', async () => {
    const boom = new Error('nope');
    const out = await runBulk([1, 2, 3], async (n) => {
      if (n === 2) throw boom;
      return n;
    });
    expect(out.map((o) => o.ok)).toEqual([true, false, true]);
    expect(out[1].error).toBe(boom);
  });

  it('survives every item failing', async () => {
    const out = await runBulk([1, 2], async () => {
      throw new Error('all down');
    });
    expect(out.map((o) => o.ok)).toEqual([false, false]);
  });

  it('never exceeds the concurrency cap', async () => {
    let inFlight = 0;
    let peak = 0;
    const gates = Array.from({ length: 10 }, () => deferred());
    const run = runBulk(
      gates.map((_, i) => i),
      async (i) => {
        inFlight++;
        peak = Math.max(peak, inFlight);
        await gates[i].promise;
        inFlight--;
      },
      3,
    );
    // Let the first lanes start before releasing anything.
    await Promise.resolve();
    expect(peak).toBe(3);
    gates.forEach((g) => g.resolve());
    await run;
    expect(peak).toBe(3);
  });

  it('does not spawn more lanes than there are items', async () => {
    let peak = 0;
    let inFlight = 0;
    await runBulk(
      [1, 2],
      async () => {
        inFlight++;
        peak = Math.max(peak, inFlight);
        await Promise.resolve();
        inFlight--;
      },
      8,
    );
    expect(peak).toBeLessThanOrEqual(2);
  });

  it('handles an empty selection without calling the worker', async () => {
    let calls = 0;
    const out = await runBulk([], async () => void calls++);
    expect(out).toEqual([]);
    expect(calls).toBe(0);
  });
});

describe('failures', () => {
  it('picks only the failed outcomes, in order', () => {
    const out = [
      { item: 'a', ok: true },
      { item: 'b', ok: false, error: 1 },
      { item: 'c', ok: false, error: 2 },
    ];
    expect(failures(out).map((o) => o.item)).toEqual(['b', 'c']);
  });
});
