// @vitest-environment jsdom
import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { MemoryRouter, Route, Routes } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, describe, expect, it } from 'vitest';
import type { MachineState, MachineSummary, Me } from '../../api/client';
import { MachineDetail } from './MachineDetail';

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const ID = 'aaaaaaaa-1111-2222-3333-444444444444';

function machine(over: Partial<MachineSummary> = {}): MachineSummary {
  return {
    id: ID,
    name: 'api-dev',
    state: 'running',
    guest_ip: '10.0.0.5',
    kernel_ref: 'vmlinux',
    rootfs_ref: 'ubuntu-24.04',
    template_id: 'go',
    resource_spec: { vcpus: 2, mem_mib: 4096, disk_mib: 10240 },
    last_error: null,
    created_at: '2026-08-18T09:00:00Z',
    last_active_at: null,
    boot: 'cold',
    disk_id: 'disk-1',
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

function render(machines: MachineSummary[], id = ID, tab = 'overview'): HTMLDivElement {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  });
  // Seeded so the Networking tab renders its form rather than its loading state.
  client.setQueryData(['network-policy', id], { mode: 'allow_all', domains: [] });
  act(() => {
    root.render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={[`/console/machines/${id}/${tab}`]}>
          <Routes>
            {/* Mirrors ConsoleApp: one route, the tab as a param. */}
            <Route
              path="/console/machines/:id/:tab"
              element={<MachineDetail me={makeMe(machines)} />}
            />
          </Routes>
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

function button(container: HTMLElement, label: string): HTMLButtonElement {
  const found = [...container.querySelectorAll('button')].find((b) => b.textContent === label);
  if (!found) throw new Error(`no button labelled ${label}`);
  return found;
}

describe('MachineDetail', () => {
  it('renders the machine it was asked for', () => {
    expect(render([machine()]).querySelector('.c-page-title')?.textContent).toBe('api-dev');
  });

  it('reports a machine that is not in the fleet', () => {
    // Terminating from another tab drops it from the shared cache.
    expect(render([], 'gone').querySelector('.c-page-title')?.textContent).toBe(
      'Machine not found',
    );
  });

  it('derives its actions the same way the table does', () => {
    const running = render([machine({ state: 'running' })]);
    expect(button(running, 'Start').disabled).toBe(true);
    expect(button(running, 'Stop').disabled).toBe(false);
    cleanup?.();
    cleanup = null;

    const stopped = render([machine({ state: 'stopped' })]);
    expect(button(stopped, 'Start').disabled).toBe(false);
    expect(button(stopped, 'Stop').disabled).toBe(true);
  });

  it('offers no lifecycle action mid-transition, but still allows terminate', () => {
    const states: MachineState[] = ['provisioning', 'stopping'];
    for (const state of states) {
      const c = render([machine({ state })]);
      expect(button(c, 'Start').disabled).toBe(true);
      expect(button(c, 'Stop').disabled).toBe(true);
      expect(button(c, 'Terminate').disabled).toBe(false);
      cleanup?.();
      cleanup = null;
    }
  });

  it('surfaces last_error as a banner rather than burying it', () => {
    const c = render([machine({ state: 'error', last_error: 'no space left on device' })]);
    expect(c.querySelector('.c-banner-error')?.textContent).toContain('no space left on device');
  });

  it('says a machine will cold boot when it has no snapshot', () => {
    expect(render([machine({ snapshot: null })]).textContent).toContain('will cold boot');
  });

  it('describes a hibernated machine’s snapshot', () => {
    // A "Stopped" machine with a snapshot resumes from memory rather than cold
    // booting — materially different to press Start on, and invisible until now.
    const c = render([
      machine({
        state: 'stopped',
        snapshot: {
          fc_version: '1.7.0',
          mem_bytes: 4 * 1024 * 1024 * 1024,
          created_at: '2026-08-20T10:00:00Z',
        },
      }),
    ]);
    expect(c.textContent).toContain('4 GiB of memory');
    expect(c.textContent).toContain('1.7.0');
  });

  it('names the backend a machine runs on', () => {
    expect(render([machine()]).textContent).toContain('Firecracker microVM');
    expect(render([machine({ backend: 'kubernetes' })]).textContent).toContain('Kubernetes pod');
  });

  it('does not offer a snapshot a Kubernetes machine can never have', () => {
    // A pod has no RAM to snapshot. An empty "Snapshot: none" row would read as
    // "not yet" — a state this machine could reach — so the row is absent, and
    // the boot line says what stopping actually does instead.
    const c = render([machine({ backend: 'kubernetes' })]);
    expect(c.textContent).not.toContain('Snapshot');
    expect(c.textContent).not.toContain('will cold boot');
    expect(c.textContent).toContain('powers this machine off');
  });

  it('marks a machine with no address rather than leaving the field blank', () => {
    expect(render([machine({ guest_ip: null })]).textContent).toContain('assigned while running');
  });

  it('opens an inline rename field', () => {
    const c = render([machine()]);
    act(() => button(c, 'Rename').click());
    const input = c.querySelector<HTMLInputElement>('input[aria-label="Machine name"]');
    expect(input?.value).toBe('api-dev');
  });

  it('closes the rename field on Escape without saving', () => {
    const c = render([machine()]);
    act(() => button(c, 'Rename').click());
    const input = c.querySelector<HTMLInputElement>('input[aria-label="Machine name"]')!;
    act(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    });
    expect(c.querySelector('input[aria-label="Machine name"]')).toBeNull();
    expect(c.querySelector('.c-page-title')?.textContent).toBe('api-dev');
  });

  it('renders the tab strip with every tab', () => {
    const labels = [...render([machine()]).querySelectorAll('.c-tab')].map((t) => t.textContent);
    expect(labels).toEqual(['Overview', 'Connect', 'Terminal', 'Networking', 'Activity']);
  });

  it('marks the tab from the URL as selected', () => {
    const c = render([machine()], ID, 'connect');
    expect(c.querySelector('.c-tab-active')?.textContent).toBe('Connect');
  });

  it('renders the connect tab, not the overview, when asked for it', () => {
    const c = render([machine()], ID, 'connect');
    expect(c.textContent).toContain('Web terminal');
    expect(c.textContent).not.toContain('Storage and boot');
  });

  it('falls back to Overview for an unknown tab', () => {
    // A stale bookmark from before a tab was renamed should land somewhere
    // meaningful rather than rendering an empty body.
    const c = render([machine()], ID, 'nope');
    expect(c.querySelector('.c-tab-active')?.textContent).toBe('Overview');
  });

  it('blocks the terminal on a stopped machine and offers to start it', () => {
    // TAV-185: the gateway's 409 is invisible to the browser, so the UI has to
    // supply the reason the protocol throws away.
    const c = render([machine({ state: 'stopped' })], ID, 'terminal');
    expect(c.textContent).toContain('Terminal unavailable');
    expect(c.textContent).toContain('Start it to open a terminal');
    expect([...c.querySelectorAll('button')].some((b) => b.textContent === 'Start machine')).toBe(
      true,
    );
  });

  it('blocks the terminal mid-transition without offering Start', () => {
    const c = render([machine({ state: 'provisioning' })], ID, 'terminal');
    expect(c.textContent).toContain('opens once it is running');
    expect([...c.querySelectorAll('button')].some((b) => b.textContent === 'Start machine')).toBe(
      false,
    );
  });

  it('tells the connect tab the terminal is unavailable too', () => {
    const c = render([machine({ state: 'stopped' })], ID, 'connect');
    expect(c.textContent).toContain('Start it to open a terminal');
    expect([...c.querySelectorAll('a')].some((a) => a.textContent === 'Open terminal')).toBe(false);
  });

  it('offers the terminal link from connect when the machine is running', () => {
    const c = render([machine({ state: 'running' })], ID, 'connect');
    expect([...c.querySelectorAll('a')].some((a) => a.textContent === 'Open terminal')).toBe(true);
  });

  it('shows this machine’s own ssh command', () => {
    const c = render([machine()], ID, 'connect');
    expect(c.textContent).toContain(`proteos ssh ${ID}`);
    expect(c.textContent).toContain(`ProxyCommand proteos ssh-proxy ${ID}`);
  });

  it('renders the networking tab with the policy modes', () => {
    const c = render([machine()], ID, 'networking');
    expect(c.textContent).toContain('Allow all');
    expect(c.textContent).toContain('Deny all');
    expect(c.querySelectorAll('input[name="network-mode"]')).toHaveLength(4);
  });

  it('says the per-machine activity feed only covers this session', () => {
    // There is no per-machine events endpoint and event rows carry no machine
    // column, so attribution exists only for events seen live. The empty state
    // must not imply the machine has simply been quiet.
    const c = render([machine()], ID, 'activity');
    expect(c.textContent).toContain('since this page was opened');
  });
});
