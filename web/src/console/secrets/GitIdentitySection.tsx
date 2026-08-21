import { useState } from 'react';
import { useGitIdentity, useGitIdentityMutations } from '../../api/hooks';
import { describeApiError } from '../errors';

// GitIdentitySection sets the name and email your machines commit with.
//
// Not a secret, but it belongs here: it is the last piece of "what identity do
// my machines act as", alongside the keys and tokens above.
//
// The API reports an effective identity plus its source — "profile" when the
// user set one, "github" when it is derived from the linked account. Showing
// the source matters because the GitHub-derived default is invisible until it
// turns up in a commit, and by then the commit is already authored wrong.
export function GitIdentitySection() {
  const identity = useGitIdentity();
  const { set, clear } = useGitIdentityMutations();
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState('');
  const [email, setEmail] = useState('');
  const [error, setError] = useState<string | null>(null);

  const current = identity.data;

  const open = () => {
    setName(current?.name ?? '');
    setEmail(current?.email ?? '');
    setError(null);
    setEditing(true);
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim() || !email.trim()) {
      setError('Both a name and an email are required.');
      return;
    }
    set.mutate(
      { name: name.trim(), email: email.trim() },
      {
        onSuccess: () => setEditing(false),
        onError: (err) => setError(describeApiError(err, 'Could not save the identity.')),
      },
    );
  };

  return (
    <div className="c-panel">
      <h2 className="c-section-title">Git identity</h2>
      <p className="c-muted">
        The name and email written to <code>~/.gitconfig</code> on your machines, and stamped on
        every commit they make.
      </p>

      {identity.isLoading && <p className="c-muted c-keys-empty">Loading…</p>}

      {current && !editing && (
        <>
          <dl className="c-defs c-identity-defs">
            <dt>Name</dt>
            <dd>{current.name || <span className="c-muted">Not set</span>}</dd>
            <dt>Email</dt>
            <dd>{current.email || <span className="c-muted">Not set</span>}</dd>
            <dt>Source</dt>
            <dd>
              {current.source === 'profile' ? (
                'Set by you'
              ) : (
                <span className="c-muted">Derived from your linked GitHub account</span>
              )}
            </dd>
          </dl>
          <div className="c-form-actions c-form-actions-left">
            <button type="button" className="c-btn" onClick={open}>
              {current.source === 'profile' ? 'Edit' : 'Override'}
            </button>
            {current.source === 'profile' && (
              <button
                type="button"
                className="c-btn"
                disabled={clear.isPending}
                onClick={() =>
                  clear.mutate(undefined, {
                    onError: (err) => setError(describeApiError(err, 'Could not clear.')),
                  })
                }
              >
                {clear.isPending ? 'Reverting…' : 'Revert to GitHub'}
              </button>
            )}
          </div>
        </>
      )}

      {editing && (
        <form className="c-secret-form" onSubmit={submit}>
          <label className="c-field">
            <span className="c-field-label">Name</span>
            <input
              type="text"
              className="c-input c-input-wide"
              value={name}
              onChange={(e) => {
                setError(null);
                setName(e.target.value);
              }}
              aria-label="Git name"
            />
          </label>
          <label className="c-field">
            <span className="c-field-label">Email</span>
            <input
              type="email"
              className="c-input c-input-wide"
              value={email}
              onChange={(e) => {
                setError(null);
                setEmail(e.target.value);
              }}
              aria-label="Git email"
            />
          </label>
          <div className="c-form-actions">
            <button type="button" className="c-btn" onClick={() => setEditing(false)}>
              Cancel
            </button>
            <button type="submit" className="c-btn c-btn-primary" disabled={set.isPending}>
              {set.isPending ? 'Saving…' : 'Save identity'}
            </button>
          </div>
        </form>
      )}

      {error && (
        <p className="c-banner c-banner-error" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
