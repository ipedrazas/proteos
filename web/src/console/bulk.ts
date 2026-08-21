// bulk runs one action across a selection of machines.
//
// It exists because the control plane has no batch lifecycle endpoint (TAV-184).
// The only bulk routes are DELETE /api/machines, which destroys EVERY machine
// the user owns rather than a chosen subset, and POST /api/machines/fill. So a
// selection-driven action bar has to fan out the per-machine calls itself.
//
// Two properties the console depends on:
//
//   1. It never rejects. One machine failing to stop must not abandon the other
//      four — the caller gets an outcome per item and decides what to report.
//   2. Results come back in input order regardless of completion order, so the
//      summary lines up with the rows the user selected.
//
// Concurrency is capped because the fan-out is the user's whole selection: a
// careless Promise.all over twenty machines is twenty simultaneous writes at
// the control plane, and the rate limiter would start rejecting them.

export interface BulkOutcome<T> {
  item: T;
  ok: boolean;
  // error is whatever the worker threw — an ApiError for a rejected call. Left
  // unknown so this module stays free of API types.
  error?: unknown;
}

const DEFAULT_BULK_CONCURRENCY = 4;

export async function runBulk<T>(
  items: readonly T[],
  worker: (item: T) => Promise<unknown>,
  concurrency: number = DEFAULT_BULK_CONCURRENCY,
): Promise<BulkOutcome<T>[]> {
  const outcomes: BulkOutcome<T>[] = new Array(items.length);
  // A shared cursor rather than fixed slices: machines finish at wildly
  // different speeds (a stop is quick, a destroy exports sessions first), so
  // fixed slices would leave workers idle behind one slow item.
  let next = 0;

  const drain = async (): Promise<void> => {
    for (;;) {
      const i = next++;
      if (i >= items.length) return;
      const item = items[i];
      try {
        await worker(item);
        outcomes[i] = { item, ok: true };
      } catch (error) {
        outcomes[i] = { item, ok: false, error };
      }
    }
  };

  const lanes = Math.max(1, Math.min(concurrency, items.length));
  await Promise.all(Array.from({ length: lanes }, drain));
  return outcomes;
}

// failures picks the outcomes that did not succeed, preserving order.
export function failures<T>(outcomes: readonly BulkOutcome<T>[]): BulkOutcome<T>[] {
  return outcomes.filter((o) => !o.ok);
}
