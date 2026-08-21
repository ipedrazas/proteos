// @vitest-environment jsdom
import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Me, SSHKeyStatus, SSHLoginKey } from '../../api/client';
import { KeysPage } from './KeysPage';

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function makeMe(sshEnabled: boolean): Me {
  return {
    user: { login: 'ada', email: 'ada@example.com', avatar_url: '' },
    prefs: { download_as_is: false, claude_attribution: true, ssh_enabled: sshEnabled },
    machines: [],
    machine_limit: 5,
    github_connected: true,
    is_admin: false,
  };
}

const KEY: SSHLoginKey = {
  id: 'k1',
  label: 'laptop',
  public_key: 'ssh-ed25519 AAAA... ada@laptop',
  fingerprint: 'SHA256:abcdef',
  created_at: '2026-08-18T09:00:00Z',
};

let cleanup: (() => void) | null = null;
afterEach(() => {
  cleanup?.();
  cleanup = null;
  vi.restoreAllMocks();
});

function render(opts: {
  sshEnabled?: boolean;
  keys?: SSHLoginKey[];
  gitKey?: SSHKeyStatus;
}): HTMLDivElement {
  const me = makeMe(opts.sshEnabled ?? false);
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  });
  // Seed every query the page reads, so nothing reaches the network in jsdom.
  client.setQueryData(['me'], me);
  client.setQueryData(['ssh-login-keys'], opts.keys ?? []);
  client.setQueryData(['ssh-key'], opts.gitKey ?? { present: false });
  client.setQueryData(['machines'], []);

  act(() => {
    root.render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <KeysPage me={me} />
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

function setTextarea(container: HTMLElement, value: string) {
  const el = container.querySelector<HTMLTextAreaElement>('textarea[aria-label="Public key"]')!;
  const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
  act(() => {
    setter.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  });
}

describe('KeysPage', () => {
  it('shows the account switch as disabled by default', () => {
    const c = render({ sshEnabled: false });
    expect(c.textContent).toContain('Disabled');
    expect(button(c, 'Turn on')).toBeTruthy();
  });

  it('asks for confirmation before turning SSH off, spelling out both effects', () => {
    // Off is a real kill-switch: keys leave every machine AND the tunnel is
    // refused. A bare "are you sure?" would undersell it.
    const c = render({ sshEnabled: true });
    act(() => button(c, 'Turn off').click());
    const dialog = document.querySelector('.c-dialog')!;
    expect(dialog.textContent).toContain('removed from every machine');
    expect(dialog.textContent).toContain('refused');
  });

  it('turns SSH on without a confirmation step', () => {
    const c = render({ sshEnabled: false });
    act(() => button(c, 'Turn on').click());
    expect(document.querySelector('.c-dialog')).toBeNull();
  });

  it('lists registered keys with their fingerprint', () => {
    const c = render({ keys: [KEY] });
    expect(c.textContent).toContain('laptop');
    expect(c.querySelector('.c-fingerprint')?.textContent).toBe('SHA256:abcdef');
  });

  it('says plainly when there is nothing to log in with', () => {
    expect(render({ keys: [] }).textContent).toContain('nothing to log in with');
  });

  it('notes that saved keys are inert while the switch is off', () => {
    const c = render({ sshEnabled: false, keys: [KEY] });
    expect(c.textContent).toContain('saved but not in use');
  });

  it('refuses a pasted private key without sending it anywhere', () => {
    // The whole point of validating client-side: the server would reject it
    // too, but only after the secret has crossed the network.
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const c = render({});
    act(() => button(c, 'Add key').click());
    setTextarea(
      c,
      '-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1r\n-----END OPENSSH PRIVATE KEY-----',
    );
    act(() => button(c, 'Add key').click());
    expect(c.textContent).toContain('never paste or share it');
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('rejects a malformed key before the round trip', () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const c = render({});
    act(() => button(c, 'Add key').click());
    setTextarea(c, 'not-a-key');
    act(() => button(c, 'Add key').click());
    expect(c.textContent).toContain('Unrecognised key type');
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('fills the label from the key comment', () => {
    const c = render({});
    act(() => button(c, 'Add key').click());
    setTextarea(
      c,
      'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJb1Kx8QGvRk9pT2mZq4wX7yNcLdEfGhIjKlMnOpQrSt ada@laptop',
    );
    const label = c.querySelector<HTMLInputElement>('input[aria-label="Key label"]');
    expect(label?.value).toBe('ada@laptop');
  });

  it('keeps the inbound and outbound keys as separate, labelled sections', () => {
    // Conflating them is the easiest mistake on this page.
    const c = render({});
    const titles = [...c.querySelectorAll('.c-section-title')].map((t) => t.textContent);
    expect(titles).toContain('Your login keys (inbound)');
    expect(titles).toContain('Machine git key (outbound)');
  });

  it('offers to generate the git key only when there is none', () => {
    const none = render({ gitKey: { present: false } });
    expect(button(none, 'Generate key')).toBeTruthy();
    cleanup?.();
    cleanup = null;

    const present = render({
      gitKey: {
        present: true,
        public_key: 'ssh-ed25519 AAAA... proteos',
        fingerprint: 'SHA256:zz',
      },
    });
    expect(present.textContent).toContain('ssh-ed25519 AAAA... proteos');
    expect(button(present, 'Replace key')).toBeTruthy();
  });

  it('warns that these commands are refused while SSH is off', () => {
    expect(render({ sshEnabled: false }).textContent).toContain('will be refused');
  });
});
