// @vitest-environment jsdom
import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { MemoryRouter, Route, Routes } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { Me } from '../api/client';
import { ConsoleApp } from './ConsoleApp';

// React needs this flag to recognize our act(...) wrapping under vitest.
(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// jsdom has no EventSource, and ConsoleApp opens the machines SSE stream on
// mount. Stub it rather than moving the subscription: one stream for the whole
// console is the design (see ConsoleApp), and the stub lets the tests assert
// that it really is one.
class FakeEventSource {
  static opened: string[] = [];
  static closed = 0;
  constructor(url: string) {
    FakeEventSource.opened.push(url);
  }
  addEventListener() {}
  removeEventListener() {}
  close() {
    FakeEventSource.closed += 1;
  }
}
(globalThis as unknown as { EventSource: unknown }).EventSource = FakeEventSource;

// Phase 0 ships the console shell with no service pages, so what is worth
// pinning is exactly the shell's contract: /console resolves to a real page
// rather than a blank frame, an unknown console path stays inside the console
// instead of bouncing to another shell, and the admin entry is gated on the
// role. Those three are the parts a later phase can silently break while every
// page still renders fine on its own.

const MACHINE = {
  id: 'aaaaaaaa-1111-2222-3333-444444444444',
  name: 'api-dev',
  state: 'running' as const,
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
};

function makeMe(overrides: Partial<Me> = {}): Me {
  return {
    user: { login: 'ada', email: 'ada@example.com', avatar_url: '' },
    prefs: { download_as_is: false, claude_attribution: true, ssh_enabled: false },
    machines: [],
    machine_limit: 5,
    github_connected: true,
    is_admin: false,
    ...overrides,
  };
}

let cleanup: (() => void) | null = null;

beforeEach(() => {
  FakeEventSource.opened = [];
  FakeEventSource.closed = 0;
});

afterEach(() => {
  cleanup?.();
  cleanup = null;
});

function render(me: Me, path: string): HTMLDivElement {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  // A fresh QueryClient per render: ConsoleApp's TopBar holds a logout mutation,
  // and sharing a client would leak state between cases.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  act(() => {
    root.render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route path="/console/*" element={<ConsoleApp me={me} />} />
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

function pageTitle(container: HTMLElement): string {
  return container.querySelector('.c-page-title')?.textContent ?? '';
}

function navLabels(container: HTMLElement): string[] {
  return [...container.querySelectorAll('.c-nav-item')].map((el) => el.textContent ?? '');
}

describe('ConsoleApp', () => {
  it('lands on Machines when opened at /console', () => {
    expect(pageTitle(render(makeMe(), '/console'))).toBe('Machines');
  });

  it('renders the requested service page', () => {
    expect(pageTitle(render(makeMe(), '/console/secrets'))).toBe('Secrets');
  });

  it('keeps an unknown console path inside the console', () => {
    expect(pageTitle(render(makeMe(), '/console/nope'))).toBe('Machines');
  });

  it('marks the active nav entry', () => {
    const container = render(makeMe(), '/console/keys');
    const active = [...container.querySelectorAll('.c-nav-item-active')].map(
      (el) => el.textContent,
    );
    expect(active).toEqual(['SSH keys']);
  });

  it('hides the Admin nav entry from a non-admin', () => {
    expect(navLabels(render(makeMe(), '/console'))).not.toContain('Admin');
  });

  it('shows the Admin nav entry to an admin', () => {
    expect(navLabels(render(makeMe({ is_admin: true }), '/console'))).toContain('Admin');
  });

  it('routes /machines/launch to the wizard, not to a machine called "launch"', () => {
    // A static segment outranks a dynamic sibling, but the failure mode if it
    // ever stopped doing so is "Machine not found" — which reads as missing
    // data, not as a routing bug. Worth pinning.
    expect(pageTitle(render(makeMe(), '/console/machines/launch'))).toBe('Launch machine');
  });

  it('renders a machine detail page for a known id', () => {
    const c = render(makeMe({ machines: [MACHINE] }), `/console/machines/${MACHINE.id}/overview`);
    expect(pageTitle(c)).toBe('api-dev');
  });

  it('redirects the detail root to the overview tab', () => {
    const c = render(makeMe({ machines: [MACHINE] }), `/console/machines/${MACHINE.id}`);
    expect(pageTitle(c)).toBe('api-dev');
  });

  it('says so when a machine id does not exist', () => {
    expect(pageTitle(render(makeMe(), '/console/machines/nope/overview'))).toBe(
      'Machine not found',
    );
  });

  it('opens exactly one machine-event stream for the whole console', () => {
    // Every console route wants live machine state, so the subscription sits in
    // the shell. One per mount — not one per page — and it closes on unmount.
    render(makeMe(), '/console/secrets');
    expect(FakeEventSource.opened).toHaveLength(1);
    cleanup?.();
    cleanup = null;
    expect(FakeEventSource.closed).toBe(1);
  });

  it('serves the Admin route to a non-admin rather than bouncing them', () => {
    // The server is the access check; a non-admin who types the URL should see
    // the page say so, not be silently redirected somewhere else.
    expect(pageTitle(render(makeMe(), '/console/admin'))).toBe('Admin');
  });
});
