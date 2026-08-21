import { useState } from 'react';
import { CLAUDE_OAUTH_KEY } from '../../api/client';
import { useProfileItems, useProfileMutations } from '../../api/hooks';
import { describeApiError } from '../errors';
import { CodeBlock } from '../ui/CodeBlock';
import { describeProfileItem } from './status';

// ClaudeSection connects a Claude subscription instead of an API key.
//
// The token comes from `claude setup-token` on the user's own machine, so the
// section's job is mostly to say that clearly — there is no OAuth redirect to
// offer, and a user who does not know the command has no way to guess it.
//
// Unlike a provider API key, this one expires. describeProfileItem folds the
// item's three flags into one status so an expiring subscription is visible
// before the agent starts failing on a machine.
export function ClaudeSection() {
  const items = useProfileItems();
  const { setItem, deleteItem } = useProfileMutations();
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState('');
  const [error, setError] = useState<string | null>(null);

  const item = items.data?.find((i) => i.key === CLAUDE_OAUTH_KEY);
  const status = describeProfileItem(item, Date.now());

  const close = () => {
    setEditing(false);
    setValue('');
    setError(null);
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!value.trim()) {
      setError('Paste the token from `claude setup-token`.');
      return;
    }
    setItem.mutate(
      { key: CLAUDE_OAUTH_KEY, value: value.trim() },
      {
        onSuccess: close,
        onError: (err) => setError(describeApiError(err, 'Could not save the token.')),
      },
    );
  };

  return (
    <div className="c-panel">
      <h2 className="c-section-title">Claude subscription</h2>
      <p className="c-muted">
        Use a Claude Pro or Max subscription on your machines instead of an Anthropic API key.
        Generate the token on your own computer — ProteOS cannot do it for you.
      </p>

      <div className="c-secret-head c-secret-head-standalone">
        <span
          className={
            status.state === 'connected'
              ? 'c-badge c-badge-ok'
              : status.state === 'attention'
                ? 'c-badge c-badge-pending'
                : 'c-badge'
          }
        >
          <span className="c-badge-dot" aria-hidden />
          {status.label}
        </span>
        <div className="c-toolbar-spacer" />
        {!editing && (
          <>
            <button type="button" className="c-btn" onClick={() => setEditing(true)}>
              {item?.connected ? 'Replace token' : 'Connect'}
            </button>
            {item?.connected && (
              <button
                type="button"
                className="c-btn c-btn-danger"
                disabled={deleteItem.isPending}
                onClick={() =>
                  deleteItem.mutate(CLAUDE_OAUTH_KEY, {
                    onError: (err) => setError(describeApiError(err, 'Could not disconnect.')),
                  })
                }
              >
                Disconnect
              </button>
            )}
          </>
        )}
      </div>

      {editing && (
        <form className="c-secret-form" onSubmit={submit}>
          <p className="c-field-label">Run this on your own computer, then paste the output</p>
          <CodeBlock text="claude setup-token" label="Copy command" />
          <label className="c-field">
            <span className="c-field-label">Token</span>
            <input
              type="password"
              className="c-input c-input-wide"
              value={value}
              onChange={(e) => {
                setError(null);
                setValue(e.target.value);
              }}
              aria-label="Claude subscription token"
              autoComplete="off"
            />
          </label>
          <div className="c-form-actions">
            <button type="button" className="c-btn" onClick={close}>
              Cancel
            </button>
            <button type="submit" className="c-btn c-btn-primary" disabled={setItem.isPending}>
              {setItem.isPending ? 'Saving…' : 'Save token'}
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
