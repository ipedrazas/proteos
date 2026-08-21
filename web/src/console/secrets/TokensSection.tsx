import { useState } from 'react';
import type { AccessToken, CreatedToken } from '../../api/client';
import { useTokenMutations, useTokens } from '../../api/hooks';
import { formatAge, formatTimestamp } from '../../lib/format';
import { describeApiError } from '../errors';
import { CodeBlock } from '../ui/CodeBlock';
import { Dialog } from '../ui/Dialog';
import { describeExpiry } from './status';

// TokensSection mints and revokes personal access tokens for the CLI.
//
// This is the one place in the console that shows a secret, and it shows it
// exactly once — the control plane returns the plaintext only in the create
// response and stores a hash, so there is genuinely no way to recover it. The
// banner therefore says "store it now" rather than the softer "you can find
// this later" that every other credential UI can afford.
export function TokensSection() {
  const tokens = useTokens();
  const { create } = useTokenMutations();
  const [name, setName] = useState('');
  const [days, setDays] = useState('');
  const [created, setCreated] = useState<CreatedToken | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [revoking, setRevoking] = useState<AccessToken | null>(null);

  const rows = tokens.data ?? [];
  const now = Date.now();

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) {
      setError('Give the token a name so you can tell them apart later.');
      return;
    }
    const expiresInDays = days.trim() === '' ? undefined : Number(days);
    if (expiresInDays !== undefined && (!Number.isFinite(expiresInDays) || expiresInDays < 1)) {
      setError('Expiry must be a number of days, or blank for no expiry.');
      return;
    }
    create.mutate(
      { name: trimmed, expiresInDays },
      {
        onSuccess: (tok) => {
          setCreated(tok);
          setName('');
          setDays('');
          setError(null);
        },
        onError: (err) => setError(describeApiError(err, 'Could not create the token.')),
      },
    );
  };

  return (
    <div className="c-panel">
      <h2 className="c-section-title">Access tokens</h2>
      <p className="c-muted">
        Authenticate the <code>proteos</code> CLI. Set one as <code>PROTEOS_TOKEN</code>, or sign in
        with <code>proteos auth login</code>.
      </p>

      {created && (
        <div className="c-token-reveal" role="status">
          <p className="c-token-reveal-title">
            Copy <strong>{created.name}</strong> now — this is the only time it is shown.
          </p>
          <CodeBlock text={created.token} label="Copy access token" />
          <button type="button" className="c-btn" onClick={() => setCreated(null)}>
            Done
          </button>
        </div>
      )}

      {rows.length > 0 && (
        <table className="c-table c-keys-table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Prefix</th>
              <th>Created</th>
              <th>Last used</th>
              <th>Expiry</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {rows.map((t) => {
              const expiry = describeExpiry(t.expires_at, now);
              return (
                <tr key={t.id} className="c-row">
                  <td>{t.name}</td>
                  <td className="c-fingerprint">{t.prefix}…</td>
                  <td className="c-nowrap">{formatTimestamp(t.created_at)}</td>
                  <td className="c-nowrap">
                    {t.last_used_at ? (
                      formatAge(now - Date.parse(t.last_used_at))
                    ) : (
                      <span className="c-muted">Never</span>
                    )}
                  </td>
                  <td className="c-nowrap">
                    <span
                      className={
                        expiry.state === 'expired' || expiry.state === 'soon'
                          ? 'c-expiry c-expiry-warn'
                          : 'c-expiry'
                      }
                    >
                      {expiry.label}
                    </span>
                  </td>
                  <td className="c-col-action">
                    <button
                      type="button"
                      className="c-btn c-btn-danger"
                      onClick={() => setRevoking(t)}
                    >
                      Revoke
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}

      {rows.length === 0 && !tokens.isLoading && (
        <p className="c-muted c-keys-empty">No tokens yet.</p>
      )}

      <form className="c-token-form" onSubmit={submit}>
        <label className="c-field c-field-inline">
          <span className="c-field-label">Name</span>
          <input
            type="text"
            className="c-input"
            value={name}
            onChange={(e) => {
              setError(null);
              setName(e.target.value);
            }}
            placeholder="laptop CLI"
            aria-label="Token name"
          />
        </label>
        <label className="c-field c-field-inline">
          <span className="c-field-label">Expires in (days)</span>
          {/* No `min` attribute: native constraint validation silently blocks
              submission, so a typed 0 would produce a no-op with no visible
              reason. The check below owns this, and says what is wrong. */}
          <input
            type="number"
            className="c-input c-input-narrow"
            value={days}
            onChange={(e) => {
              setError(null);
              setDays(e.target.value);
            }}
            placeholder="never"
            aria-label="Token expiry in days"
          />
        </label>
        <button type="submit" className="c-btn c-btn-primary" disabled={create.isPending}>
          {create.isPending ? 'Creating…' : 'Create token'}
        </button>
      </form>

      {error && (
        <p className="c-banner c-banner-error" role="alert">
          {error}
        </p>
      )}

      {revoking && <RevokeTokenDialog token={revoking} onClose={() => setRevoking(null)} />}
    </div>
  );
}

function RevokeTokenDialog({ token, onClose }: { token: AccessToken; onClose: () => void }) {
  const { revoke } = useTokenMutations();
  const [error, setError] = useState<string | null>(null);

  return (
    <Dialog title="Revoke token" onClose={onClose} dismissable={!revoke.isPending}>
      <p className="c-dialog-warning">
        Revoke <strong>{token.name}</strong>? Anything using it — a CLI on another machine, a
        script, a CI job — stops working immediately.
      </p>
      {error && (
        <p className="c-banner c-banner-error" role="alert">
          {error}
        </p>
      )}
      <div className="c-dialog-actions">
        <button type="button" className="c-btn" onClick={onClose}>
          Cancel
        </button>
        <button
          type="button"
          className="c-btn c-btn-danger"
          disabled={revoke.isPending}
          onClick={() =>
            revoke.mutate(token.id, {
              onSuccess: onClose,
              onError: (e) => setError(describeApiError(e, 'Could not revoke the token.')),
            })
          }
        >
          {revoke.isPending ? 'Revoking…' : 'Revoke'}
        </button>
      </div>
    </Dialog>
  );
}
