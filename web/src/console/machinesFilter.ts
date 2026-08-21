// machinesFilter is the machines table's search and state filter.
//
// Both are pure and separated from the table so the matching rules can be
// pinned by tests: they are the kind of thing that quietly stops working (a
// case-sensitive compare, a null template throwing) without any visible error —
// the table just shows fewer rows than it should, which reads as missing data.

import type { MachineState, MachineSummary } from '../api/client';
import { describeState } from './machineState';

// StateFilter groups the API's states the same way the badges do, so the filter
// offers the four things a reader can actually see in the table.
export type StateFilter = 'all' | 'running' | 'pending' | 'stopped' | 'error';

export const STATE_FILTERS: { value: StateFilter; label: string }[] = [
  { value: 'all', label: 'All states' },
  { value: 'running', label: 'Running' },
  { value: 'pending', label: 'Pending' },
  { value: 'stopped', label: 'Stopped' },
  { value: 'error', label: 'Error' },
];

export function matchesStateFilter(state: MachineState, filter: StateFilter): boolean {
  if (filter === 'all') return true;
  if (filter === 'pending') return describeState(state).busy;
  return state === filter;
}

// matchesQuery does a case-insensitive substring match over the fields a user
// would plausibly paste or half-remember: the name, the machine id, and the
// template it came from. Not the state — that is what the state filter is for,
// and matching it here would make "error" hide machines merely named "error-repro".
export function matchesQuery(machine: MachineSummary, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (q === '') return true;
  return (
    machine.name.toLowerCase().includes(q) ||
    machine.id.toLowerCase().includes(q) ||
    (machine.template_id ?? '').toLowerCase().includes(q)
  );
}

export function filterMachines(
  machines: readonly MachineSummary[],
  query: string,
  state: StateFilter,
): MachineSummary[] {
  return machines.filter((m) => matchesStateFilter(m.state, state) && matchesQuery(m, query));
}
