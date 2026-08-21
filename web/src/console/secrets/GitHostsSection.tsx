import { useState } from 'react';
import type { GitHost } from '../../api/client';
import { useGitHostMutations, useGitHosts } from '../../api/hooks';
import { describeApiError } from '../errors';

// GitHostsSection manages a personal access token per additional git host.
//
// The host list is operator-allowlisted, so on a deployment with none this
// section renders nothing at all rather than an empty panel explaining a
// feature that is not available — an empty state for something you cannot act
// on is just noise.
export function GitHostsSection() {
  const hosts = useGitHosts();
  const rows = hosts.data ?? [];
  if (hosts.isLoading || rows.length === 0) return null;

  return (
    <div className="c-panel">
      <h2 className="c-section-title">Git hosts</h2>
      <p className="c-muted">
        Personal access tokens for the additional git hosts this deployment allows. Write-only —
        only the account they authenticate as is readable afterwards.
      </p>
      <ul className="c-secret-list">
        {rows.map((h) => (
          <GitHostRow key={h.host} host={h} />
        ))}
      </ul>
    </div>
  );
}

function GitHostRow({ host }: { host: GitHost }) {
  const { setToken, deleteToken } = useGitHostMutations();
  const [editing, setEditing] = useState(false);
  const [token, setTokenValue] = useState('');
  const [error, setError] = useState<string | null>(null);

  const close = () => {
    setEditing(false);
    setTokenValue('');
    setError(null);
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!token.trim()) {
      setError('Paste a token.');
      return;
    }
    setToken.mutate(
      { host: host.host, token: token.trim() },
      {
        onSuccess: close,
        onError: (err) => setError(describeApiError(err, 'Could not save the token.')),
      },
    );
  };

  return (
    <li className="c-secret-row">
      <div className="c-secret-head">
        <span className="c-secret-name">{host.host}</span>
        <span className={host.linked ? 'c-badge c-badge-ok' : 'c-badge'}>
          <span className="c-badge-dot" aria-hidden />
          {host.linked ? `Linked${host.login ? ` as ${host.login}` : ''}` : 'Not linked'}
        </span>
        <div className="c-toolbar-spacer" />
        {!editing && (
          <>
            <button type="button" className="c-btn" onClick={() => setEditing(true)}>
              {host.linked ? 'Replace token' : 'Add token'}
            </button>
            {host.linked && (
              <button
                type="button"
                className="c-btn c-btn-danger"
                disabled={deleteToken.isPending}
                onClick={() =>
                  deleteToken.mutate(host.host, {
                    onError: (err) =>
                      setError(describeApiError(err, 'Could not remove the token.')),
                  })
                }
              >
                Remove
              </button>
            )}
          </>
        )}
      </div>

      {editing && (
        <form className="c-secret-form" onSubmit={submit}>
          <label className="c-field">
            <span className="c-field-label">Personal access token</span>
            <input
              type="password"
              className="c-input c-input-wide"
              value={token}
              onChange={(e) => {
                setError(null);
                setTokenValue(e.target.value);
              }}
              aria-label={`${host.host} token`}
              autoComplete="off"
            />
          </label>
          <div className="c-form-actions">
            <button type="button" className="c-btn" onClick={close}>
              Cancel
            </button>
            <button type="submit" className="c-btn c-btn-primary" disabled={setToken.isPending}>
              {setToken.isPending ? 'Saving…' : 'Save token'}
            </button>
          </div>
        </form>
      )}

      {error && (
        <p className="c-banner c-banner-error" role="alert">
          {error}
        </p>
      )}
    </li>
  );
}
