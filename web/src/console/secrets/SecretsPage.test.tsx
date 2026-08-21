// @vitest-environment jsdom
import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../api/client';
import type { AccessToken, GitHost, GitIdentity, ProfileItem, Provider } from '../../api/client';
import { SecretsPage } from './SecretsPage';

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const PROVIDER: Provider = {
  key: 'anthropic',
  display_name: 'Anthropic',
  enabled: true,
  key_set: false,
  secret_fields: [{ name: 'api_key', label: 'API key', env: 'ANTHROPIC_API_KEY' }],
};

const IDENTITY: GitIdentity = { name: 'Ada', email: 'ada@example.com', source: 'github' };

let cleanup: (() => void) | null = null;
afterEach(() => {
  cleanup?.();
  cleanup = null;
  vi.restoreAllMocks();
});

function render(seed: {
  providers?: Provider[];
  tokens?: AccessToken[];
  hosts?: GitHost[];
  items?: ProfileItem[];
  identity?: GitIdentity;
}): HTMLDivElement {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  });
  client.setQueryData(['providers'], seed.providers ?? [PROVIDER]);
  client.setQueryData(['tokens'], seed.tokens ?? []);
  client.setQueryData(['git-hosts'], seed.hosts ?? []);
  client.setQueryData(['profile-items'], seed.items ?? []);
  client.setQueryData(['git-identity'], seed.identity ?? IDENTITY);

  act(() => {
    root.render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <SecretsPage />
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

// settle flushes React Query's notify-manager scheduling plus the re-render it
// triggers. A single microtask tick is not enough.
async function settle() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 10));
  });
}

function button(container: HTMLElement, label: string): HTMLButtonElement {
  const found = [...container.querySelectorAll('button')].find((b) => b.textContent === label);
  if (!found) throw new Error(`no button labelled ${label}`);
  return found;
}

describe('SecretsPage', () => {
  it('renders a provider form from its declared fields, with no per-provider code', () => {
    // Adding a provider on the control plane must not need a frontend change.
    const c = render({
      providers: [
        {
          ...PROVIDER,
          key: 'custom',
          display_name: 'Custom LLM',
          secret_fields: [
            { name: 'api_key', label: 'API key', env: 'CUSTOM_KEY' },
            { name: 'base_url', label: 'Base URL', env: 'CUSTOM_URL' },
          ],
        },
      ],
    });
    act(() => button(c, 'Set key').click());
    expect(c.querySelector('input[aria-label="Custom LLM API key"]')).toBeTruthy();
    expect(c.querySelector('input[aria-label="Custom LLM Base URL"]')).toBeTruthy();
    expect(c.textContent).toContain('CUSTOM_URL');
  });

  it('never renders a provider key back — only whether one is set', () => {
    const c = render({ providers: [{ ...PROVIDER, key_set: true }] });
    expect(c.textContent).toContain('Key set');
    expect(c.textContent).not.toContain('sk-');
    // And the entry field is masked.
    act(() => button(c, 'Replace key').click());
    const input = c.querySelector<HTMLInputElement>('input[aria-label="Anthropic API key"]');
    expect(input?.type).toBe('password');
  });

  it('offers Remove only once a key exists', () => {
    const none = render({ providers: [{ ...PROVIDER, key_set: false }] });
    expect([...none.querySelectorAll('button')].some((b) => b.textContent === 'Remove')).toBe(
      false,
    );
    cleanup?.();
    cleanup = null;
    const set = render({ providers: [{ ...PROVIDER, key_set: true }] });
    expect(button(set, 'Remove')).toBeTruthy();
  });

  it('lists tokens by prefix only', () => {
    const c = render({
      tokens: [
        {
          id: 't1',
          name: 'laptop',
          prefix: 'pro_ab12',
          created_at: '2026-08-01T09:00:00Z',
        },
      ],
    });
    expect(c.textContent).toContain('pro_ab12');
    expect(c.textContent).toContain('laptop');
  });

  it('flags an expired token distinctly from a healthy one', () => {
    const c = render({
      tokens: [
        {
          id: 't1',
          name: 'old',
          prefix: 'pro_x',
          created_at: '2026-01-01T00:00:00Z',
          expires_at: '2026-02-01T00:00:00Z',
        },
        { id: 't2', name: 'forever', prefix: 'pro_y', created_at: '2026-01-01T00:00:00Z' },
      ],
    });
    expect(c.textContent).toContain('Expired');
    expect(c.textContent).toContain('Never expires');
    expect(c.querySelectorAll('.c-expiry-warn')).toHaveLength(1);
  });

  it('requires a token name before creating one', () => {
    const c = render({});
    act(() => button(c, 'Create token').click());
    expect(c.textContent).toContain('Give the token a name');
  });

  it('rejects a nonsensical expiry', () => {
    const c = render({});
    const name = c.querySelector<HTMLInputElement>('input[aria-label="Token name"]')!;
    const days = c.querySelector<HTMLInputElement>('input[aria-label="Token expiry in days"]')!;
    const setInput = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
    act(() => {
      setInput.call(name, 'cli');
      name.dispatchEvent(new Event('input', { bubbles: true }));
    });
    act(() => {
      setInput.call(days, '0');
      days.dispatchEvent(new Event('input', { bubbles: true }));
    });
    expect(
      c.querySelector<HTMLInputElement>('input[aria-label="Token expiry in days"]')!.value,
    ).toBe('0');
    act(() => button(c, 'Create token').click());
    expect(c.textContent).toContain('number of days');
  });

  it('hides the git hosts section entirely when the operator allows none', () => {
    // An empty state for something you cannot act on is noise.
    const c = render({ hosts: [] });
    expect(c.textContent).not.toContain('Git hosts');
  });

  it('shows git hosts and their link state when some exist', () => {
    const c = render({
      hosts: [{ host: 'git.example.com', linked: true, login: 'ada' }],
    });
    expect(c.textContent).toContain('git.example.com');
    expect(c.textContent).toContain('Linked as ada');
  });

  it('reports the Claude subscription as not connected when absent', () => {
    expect(render({ items: [] }).textContent).toContain('Not connected');
  });

  it('flags a Claude token that needs reconnecting over saying it is connected', () => {
    const c = render({
      items: [
        {
          key: 'claude-oauth',
          kind: 'oauth',
          target: 'claude',
          connected: true,
          needs_reconnect: true,
          created_at: '2026-01-01T00:00:00Z',
          updated_at: '2026-01-01T00:00:00Z',
        },
      ],
    });
    expect(c.textContent).toContain('Needs reconnecting');
  });

  it('says when the git identity is only derived from GitHub', () => {
    // The derived default is invisible until it turns up in a commit.
    const c = render({ identity: { name: 'Ada', email: 'ada@example.com', source: 'github' } });
    expect(c.textContent).toContain('Derived from your linked GitHub account');
    expect(button(c, 'Override')).toBeTruthy();
  });

  it('offers to revert only when the identity was set by the user', () => {
    const c = render({ identity: { name: 'Ada', email: 'a@b.c', source: 'profile' } });
    expect(button(c, 'Revert to GitHub')).toBeTruthy();
    expect(c.textContent).toContain('Set by you');
  });

  it('shows a new token exactly once, and never in the list', async () => {
    // The control plane stores only a hash, so there is genuinely no way to
    // recover this. If the reveal ever stopped appearing, or the list ever
    // started carrying the plaintext, both would be silent failures.
    const SECRET = 'pro_averyrealsecrettokenvalue123456';
    vi.spyOn(api, 'createToken').mockResolvedValue({
      id: 'new',
      name: 'cli',
      token: SECRET,
      prefix: 'pro_aver',
    });
    // The mutation's onSuccess invalidates the tokens query, and React Query
    // waits for that refetch before it reports success — so without a listTokens
    // that can resolve, the mutation stays pending forever in jsdom. The
    // refetched row carries the PREFIX only, which is the half of the assertion
    // that matters: the list must never hold the plaintext.
    vi.spyOn(api, 'listTokens').mockResolvedValue([
      { id: 'new', name: 'cli', prefix: 'pro_aver', created_at: '2026-08-21T09:00:00Z' },
    ]);

    const c = render({});
    const name = c.querySelector<HTMLInputElement>('input[aria-label="Token name"]')!;
    const setInput = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
    act(() => {
      setInput.call(name, 'cli');
      name.dispatchEvent(new Event('input', { bubbles: true }));
    });

    await act(async () => {
      button(c, 'Create token').click();
    });
    // React Query dispatches through its notify manager, which schedules on a
    // macrotask; the state settles a tick after the promise resolves, so a
    // microtask flush alone leaves the mutation showing as pending.
    await settle();

    expect(api.createToken).toHaveBeenCalledWith('cli', undefined);
    expect(c.textContent).toContain('only time it is shown');
    expect(c.textContent).toContain(SECRET);

    await act(async () => {
      button(c, 'Done').click();
    });
    await settle();

    expect(c.textContent).not.toContain(SECRET);
  });
});
