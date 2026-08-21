import { useEffect, useState } from 'react';
import type { MachineSummary, NetworkPolicyMode } from '../../api/client';
import { useNetworkPolicy, useSetNetworkPolicy } from '../../api/hooks';
import { describeApiError } from '../errors';

// NetworkingTab edits one machine's egress policy.
//
// A machine with no policy row behaves as allow_all, so "no policy" and
// "allow everything" are the same observable state — the UI shows allow_all
// rather than an empty form, because a blank policy screen reads as "nothing is
// configured, so nothing is allowed", which is the opposite of the truth.
const MODES: { value: NetworkPolicyMode; label: string; hint: string }[] = [
  { value: 'allow_all', label: 'Allow all', hint: 'No restrictions. This is the default.' },
  {
    value: 'deny_all',
    label: 'Deny all',
    hint: 'No outbound network at all. Package installs and git will fail.',
  },
  {
    value: 'allow_domains',
    label: 'Allow listed domains only',
    hint: 'Everything else is blocked. Remember the hosts your toolchain needs.',
  },
  {
    value: 'deny_domains',
    label: 'Deny listed domains',
    hint: 'Everything else is allowed.',
  },
];

export function NetworkingTab({ machine }: { machine: MachineSummary }) {
  const policy = useNetworkPolicy(machine.id);
  const save = useSetNetworkPolicy(machine.id);

  const [mode, setMode] = useState<NetworkPolicyMode>('allow_all');
  const [domains, setDomains] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  // Seed the form from the server's copy once it lands, and re-seed whenever the
  // server's copy changes (a save invalidates the query, so this is also how the
  // form picks up any normalisation the server applied to the domain list).
  useEffect(() => {
    if (!policy.data) return;
    setMode(policy.data.mode);
    setDomains(policy.data.domains.join('\n'));
  }, [policy.data]);

  const needsDomains = mode === 'allow_domains' || mode === 'deny_domains';
  const parsedDomains = domains
    .split(/[\s,]+/)
    .map((d) => d.trim())
    .filter(Boolean);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setSaved(false);
    if (needsDomains && parsedDomains.length === 0) {
      setError('List at least one domain, or choose a mode that does not need one.');
      return;
    }
    save.mutate(
      { mode, domains: needsDomains ? parsedDomains : [] },
      {
        onSuccess: () => setSaved(true),
        onError: (err) => setError(describeApiError(err, 'Could not save the policy.')),
      },
    );
  };

  if (policy.isLoading) {
    return (
      <div className="c-panel">
        <p className="c-muted">Loading policy…</p>
      </div>
    );
  }

  return (
    <form className="c-panel" onSubmit={submit}>
      <h2 className="c-section-title">Outbound network</h2>
      <p className="c-muted">
        Controls what this machine can reach. Applies to everything running inside it — your shell,
        the coding agents, and package managers alike.
      </p>

      <fieldset className="c-modes">
        <legend className="c-field-label">Mode</legend>
        {MODES.map((m) => (
          <label className="c-mode" key={m.value}>
            <input
              type="radio"
              name="network-mode"
              value={m.value}
              checked={mode === m.value}
              onChange={() => {
                setMode(m.value);
                setError(null);
                setSaved(false);
              }}
            />
            <span>
              <span className="c-mode-label">{m.label}</span>
              <span className="c-muted c-mode-hint">{m.hint}</span>
            </span>
          </label>
        ))}
      </fieldset>

      {needsDomains && (
        <label className="c-field">
          <span className="c-field-label">Domains</span>
          <textarea
            className="c-textarea"
            rows={5}
            value={domains}
            onChange={(e) => {
              setDomains(e.target.value);
              setError(null);
              setSaved(false);
            }}
            placeholder={'github.com\nregistry.npmjs.org\nproxy.golang.org'}
            aria-label="Domains"
          />
          <span className="c-muted c-field-hint">
            One per line. {parsedDomains.length} {parsedDomains.length === 1 ? 'domain' : 'domains'}
            .
          </span>
        </label>
      )}

      {error && (
        <p className="c-banner c-banner-error" role="alert">
          {error}
        </p>
      )}
      {saved && !error && (
        <p className="c-banner c-banner-ok" role="status">
          Policy saved.
        </p>
      )}

      <div className="c-form-actions c-form-actions-left">
        <button type="submit" className="c-btn c-btn-primary" disabled={save.isPending}>
          {save.isPending ? 'Saving…' : 'Save policy'}
        </button>
      </div>
    </form>
  );
}
