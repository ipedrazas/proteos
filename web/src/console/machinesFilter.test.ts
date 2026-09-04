import { describe, expect, it } from 'vitest';
import type { MachineState, MachineSummary } from '../api/client';
import { filterMachines, matchesQuery, matchesStateFilter } from './machinesFilter';

function machine(over: Partial<MachineSummary> = {}): MachineSummary {
  return {
    id: '11111111-2222-3333-4444-555555555555',
    name: 'dev-1',
    state: 'running',
    guest_ip: null,
    kernel_ref: 'vmlinux',
    rootfs_ref: 'ubuntu-24.04',
    template_id: 'go',
    resource_spec: { vcpus: 2, mem_mib: 4096, disk_mib: 10240 },
    last_error: null,
    created_at: '2026-08-18T09:00:00Z',
    last_active_at: null,
    boot: null,
    disk_id: null,
    disk_mib: null,
    snapshot: null,
    backend: 'firecracker',
    ...over,
  };
}

describe('matchesStateFilter', () => {
  it('passes everything under "all"', () => {
    for (const s of ['running', 'stopped', 'error', 'provisioning'] as MachineState[]) {
      expect(matchesStateFilter(s, 'all')).toBe(true);
    }
  });

  it('matches the settled states exactly', () => {
    expect(matchesStateFilter('running', 'running')).toBe(true);
    expect(matchesStateFilter('stopped', 'running')).toBe(false);
    expect(matchesStateFilter('error', 'error')).toBe(true);
  });

  it('groups every transitional state under "pending"', () => {
    const transitional: MachineState[] = [
      'requested',
      'provisioning',
      'starting',
      'stopping',
      'hibernating',
    ];
    for (const s of transitional) expect(matchesStateFilter(s, 'pending')).toBe(true);
    expect(matchesStateFilter('running', 'pending')).toBe(false);
    expect(matchesStateFilter('stopped', 'pending')).toBe(false);
  });
});

describe('matchesQuery', () => {
  it('passes everything for a blank or whitespace query', () => {
    expect(matchesQuery(machine(), '')).toBe(true);
    expect(matchesQuery(machine(), '   ')).toBe(true);
  });

  it('matches a name substring, case-insensitively', () => {
    expect(matchesQuery(machine({ name: 'Build-Box' }), 'build')).toBe(true);
    expect(matchesQuery(machine({ name: 'Build-Box' }), 'BOX')).toBe(true);
    expect(matchesQuery(machine({ name: 'Build-Box' }), 'jenkins')).toBe(false);
  });

  it('matches an id prefix, so a pasted id finds its row', () => {
    expect(matchesQuery(machine({ id: 'abc12345-0000' }), 'abc12345')).toBe(true);
  });

  it('matches the template', () => {
    expect(matchesQuery(machine({ template_id: 'python' }), 'pyth')).toBe(true);
  });

  it('tolerates a machine with no template', () => {
    // Legacy machines predate templates; template_id is null for them.
    expect(() => matchesQuery(machine({ template_id: null }), 'go')).not.toThrow();
    expect(matchesQuery(machine({ template_id: null }), 'go')).toBe(false);
  });

  it('ignores state, leaving that to the state filter', () => {
    // Otherwise "error" would hide a machine merely named error-repro.
    expect(matchesQuery(machine({ name: 'web', state: 'error' }), 'error')).toBe(false);
    expect(matchesQuery(machine({ name: 'error-repro', state: 'running' }), 'error')).toBe(true);
  });
});

describe('filterMachines', () => {
  const machines = [
    machine({ id: 'a', name: 'api', state: 'running', template_id: 'go' }),
    machine({ id: 'b', name: 'web', state: 'stopped', template_id: 'node' }),
    machine({ id: 'c', name: 'api-2', state: 'provisioning', template_id: 'go' }),
  ];

  it('applies both filters together', () => {
    expect(filterMachines(machines, 'api', 'running').map((m) => m.id)).toEqual(['a']);
  });

  it('preserves input order', () => {
    expect(filterMachines(machines, '', 'all').map((m) => m.id)).toEqual(['a', 'b', 'c']);
  });

  it('returns nothing when the two filters cannot both hold', () => {
    expect(filterMachines(machines, 'web', 'running')).toEqual([]);
  });
});
