// @vitest-environment jsdom
import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, describe, expect, it } from 'vitest';
import type { MachineState, MachineSummary, Me } from '../../api/client';
import { MachinesList } from './MachinesList';

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// What is worth pinning here is the action bar's enablement, because it is the
// guard between a selection and a destructive call. "Terminate is enabled" and
// "Start is disabled" are the difference between destroying two machines and
// destroying five, and neither is visible in a type.

function machine(over: Partial<MachineSummary> = {}): MachineSummary {
  return {
    id: 'id-1',
    name: 'dev-1',
    state: 'running',
    guest_ip: null,
    kernel_ref: 'vmlinux',
    rootfs_ref: 'ubuntu-24.04',
    template_id: 'go',
    resource_spec: { vcpus: 2, mem_mib: 4096, disk_mib: 10240 },
    last_error: null,
    created_at: new Date(Date.now() - 3600_000).toISOString(),
    last_active_at: null,
    boot: null,
    disk_id: null,
    disk_mib: 10240,
    snapshot: null,
    backend: 'firecracker',
    ...over,
  };
}

function makeMe(machines: MachineSummary[]): Me {
  return {
    user: { login: 'ada', email: 'ada@example.com', avatar_url: '' },
    prefs: { download_as_is: false, claude_attribution: true, ssh_enabled: false },
    machines,
    machine_limit: 5,
    github_connected: true,
    is_admin: false,
  };
}

let cleanup: (() => void) | null = null;
afterEach(() => {
  cleanup?.();
  cleanup = null;
});

function render(machines: MachineSummary[]): HTMLDivElement {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  // staleTime Infinity keeps the seeded machines authoritative — without it the
  // query refetches and the rows depend on a network call jsdom cannot make.
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  });
  act(() => {
    root.render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <MachinesList me={makeMe(machines)} />
        </MemoryRouter>
      </QueryClientProvider>,
    );
  });
  cleanup = () => {
    act(() => root.unmount());
    container.remove();
  };
  return container;
}

// Matched by prefix: Start and Stop carry a count when they apply to only part
// of the selection ("Start (2)").
function button(container: HTMLElement, label: string): HTMLButtonElement {
  const found = [...container.querySelectorAll('button')].find((b) =>
    (b.textContent ?? '').startsWith(label),
  );
  if (!found) throw new Error(`no button labelled ${label}`);
  return found;
}

function selectMachine(container: HTMLElement, name: string) {
  const box = container.querySelector<HTMLInputElement>(`input[aria-label="Select ${name}"]`);
  if (!box) throw new Error(`no checkbox for ${name}`);
  act(() => box.click());
}

function rowNames(container: HTMLElement): string[] {
  return [...container.querySelectorAll('.c-machine-name')].map((el) => el.textContent ?? '');
}

function setSelect(container: HTMLElement, label: string, value: string) {
  const el = container.querySelector<HTMLSelectElement>(`select[aria-label="${label}"]`);
  if (!el) throw new Error(`no select ${label}`);
  const setter = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value')!.set!;
  act(() => {
    setter.call(el, value);
    el.dispatchEvent(new Event('change', { bubbles: true }));
  });
}

describe('MachinesList', () => {
  it('renders a row per machine', () => {
    const c = render([machine({ id: 'a', name: 'api' }), machine({ id: 'b', name: 'web' })]);
    expect(rowNames(c)).toEqual(['api', 'web']);
  });

  it('shows the first-run panel instead of an empty table', () => {
    const c = render([]);
    expect(c.textContent).toContain('No machines yet');
    expect(c.querySelector('.c-table')).toBeNull();
  });

  it('disables every action while nothing is selected', () => {
    const c = render([machine()]);
    expect(button(c, 'Start').disabled).toBe(true);
    expect(button(c, 'Stop').disabled).toBe(true);
    expect(button(c, 'Terminate').disabled).toBe(true);
  });

  it('enables Stop but not Start for a running selection', () => {
    const c = render([machine({ name: 'api', state: 'running' })]);
    selectMachine(c, 'api');
    expect(button(c, 'Stop').disabled).toBe(false);
    expect(button(c, 'Start').disabled).toBe(true);
  });

  it('enables Start but not Stop for a stopped selection', () => {
    const c = render([machine({ name: 'api', state: 'stopped' })]);
    selectMachine(c, 'api');
    expect(button(c, 'Start').disabled).toBe(false);
    expect(button(c, 'Stop').disabled).toBe(true);
  });

  it('acts on the eligible subset of a mixed selection, and says how many', () => {
    const c = render([
      machine({ id: 'a', name: 'api', state: 'running' }),
      machine({ id: 'b', name: 'web', state: 'stopped' }),
    ]);
    selectMachine(c, 'api');
    selectMachine(c, 'web');
    // One of the two can start, the other can stop — both act on their subset,
    // and the count on the label is what makes that honest rather than silent.
    expect(button(c, 'Start').disabled).toBe(false);
    expect(button(c, 'Start').textContent).toBe('Start (1)');
    expect(button(c, 'Stop').disabled).toBe(false);
    expect(button(c, 'Stop').textContent).toBe('Stop (1)');
    expect(button(c, 'Terminate').disabled).toBe(false);
  });

  it('drops the count when the action covers the whole selection', () => {
    const c = render([
      machine({ id: 'a', name: 'api', state: 'stopped' }),
      machine({ id: 'b', name: 'web', state: 'stopped' }),
    ]);
    selectMachine(c, 'api');
    selectMachine(c, 'web');
    expect(button(c, 'Start').textContent).toBe('Start');
  });

  it('offers no lifecycle action for a busy machine', () => {
    const c = render([machine({ name: 'api', state: 'provisioning' })]);
    selectMachine(c, 'api');
    expect(button(c, 'Start').disabled).toBe(true);
    expect(button(c, 'Stop').disabled).toBe(true);
  });

  it('allows terminating a machine in error', () => {
    const c = render([machine({ name: 'api', state: 'error' })]);
    selectMachine(c, 'api');
    expect(button(c, 'Terminate').disabled).toBe(false);
    expect(button(c, 'Start').disabled).toBe(true);
  });

  it('filters rows by state', () => {
    const c = render([
      machine({ id: 'a', name: 'api', state: 'running' }),
      machine({ id: 'b', name: 'web', state: 'stopped' }),
    ]);
    setSelect(c, 'Filter by state', 'stopped');
    expect(rowNames(c)).toEqual(['web']);
  });

  it('select-all covers only the rows the filter left on screen', () => {
    // Otherwise filtering to one state and hitting the header checkbox selects
    // machines the user cannot see — which is how people terminate the wrong thing.
    const c = render([
      machine({ id: 'a', name: 'api', state: 'running' }),
      machine({ id: 'b', name: 'web', state: 'stopped' }),
      machine({ id: 'c', name: 'db', state: 'stopped' }),
    ]);
    setSelect(c, 'Filter by state', 'stopped');
    const all = c.querySelector<HTMLInputElement>('input[aria-label="Select all machines"]')!;
    act(() => all.click());
    // Two stopped machines selected; Start applies to both, Stop to none.
    expect(c.textContent).toContain('2 selected');
    expect(button(c, 'Start').disabled).toBe(false);
    expect(button(c, 'Stop').disabled).toBe(true);
  });

  it('offers a way out when the filter matches nothing', () => {
    const c = render([machine({ name: 'api', state: 'running' })]);
    setSelect(c, 'Filter by state', 'error');
    expect(c.textContent).toContain('No machines match these filters');
    act(() => button(c, 'Clear filters').click());
    expect(rowNames(c)).toEqual(['api']);
  });

  it('shows a machine with no template as an em dash rather than blank', () => {
    const c = render([machine({ name: 'api', template_id: null })]);
    expect(c.querySelector('.c-table')!.textContent).toContain('—');
  });

  it('surfaces last_error on the row', () => {
    const c = render([machine({ name: 'api', state: 'error', last_error: 'kernel panic' })]);
    expect(c.querySelector('.c-row-error')?.textContent).toBe('kernel panic');
  });

  it('describes every state it is given a badge for', () => {
    const states: MachineState[] = ['running', 'stopped', 'error', 'provisioning', 'stopping'];
    const c = render(states.map((s, i) => machine({ id: `m${i}`, name: `m${i}`, state: s })));
    expect(c.querySelectorAll('.c-badge')).toHaveLength(states.length);
  });
});
