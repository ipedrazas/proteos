import { useState } from 'react';
import type { Provider } from '../../api/client';
import { useProviderMutations, useProviders } from '../../api/hooks';
import { describeApiError } from '../errors';

// ProvidersSection manages each coding agent's API key.
//
// The form is rendered entirely from the provider's declared secret_fields —
// there is no per-provider code here, and adding a provider on the control
// plane needs no frontend change. That is a property of the API worth
// preserving rather than quietly hardcoding fields.
//
// Keys are write-only end to end: the API never returns one, so the UI can only
// ever report whether a key is set. Nothing on this page can show a key back.
export function ProvidersSection() {
  const providers = useProviders();
  const rows = providers.data ?? [];

  return (
    <div className="c-panel">
      <h2 className="c-section-title">AI providers</h2>
      <p className="c-muted">
        API keys for the coding agents. Stored encrypted and injected into your machines at runtime
        — never shown again once saved.
      </p>

      {providers.isLoading && <p className="c-muted c-keys-empty">Loading…</p>}
      {providers.isError && (
        <p className="c-banner c-banner-error" role="alert">
          Could not load providers.
        </p>
      )}
      {!providers.isLoading && rows.length === 0 && (
        <p className="c-muted c-keys-empty">No providers are registered on this deployment.</p>
      )}

      <ul className="c-secret-list">
        {rows.map((p) => (
          <ProviderRow key={p.key} provider={p} />
        ))}
      </ul>
    </div>
  );
}

function ProviderRow({ provider }: { provider: Provider }) {
  const { setKey, deleteKey } = useProviderMutations();
  const [editing, setEditing] = useState(false);
  const [values, setValues] = useState<Record<string, string>>({});
  const [error, setError] = useState<string | null>(null);

  const close = () => {
    setEditing(false);
    setValues({});
    setError(null);
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const missing = provider.secret_fields.filter((f) => !(values[f.name] ?? '').trim());
    if (missing.length > 0) {
      setError(`Fill in ${missing.map((f) => f.label).join(', ')}.`);
      return;
    }
    setKey.mutate(
      { key: provider.key, fields: values },
      {
        onSuccess: close,
        onError: (err) => setError(describeApiError(err, 'Could not save the key.')),
      },
    );
  };

  return (
    <li className="c-secret-row">
      <div className="c-secret-head">
        <span className="c-secret-name">{provider.display_name}</span>
        <span className={provider.key_set ? 'c-badge c-badge-ok' : 'c-badge'}>
          <span className="c-badge-dot" aria-hidden />
          {provider.key_set ? 'Key set' : 'No key'}
        </span>
        <div className="c-toolbar-spacer" />
        {!editing && (
          <>
            <button type="button" className="c-btn" onClick={() => setEditing(true)}>
              {provider.key_set ? 'Replace key' : 'Set key'}
            </button>
            {provider.key_set && (
              <button
                type="button"
                className="c-btn c-btn-danger"
                disabled={deleteKey.isPending}
                onClick={() =>
                  deleteKey.mutate(provider.key, {
                    onError: (err) => setError(describeApiError(err, 'Could not remove the key.')),
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
          {provider.secret_fields.map((field) => (
            <label className="c-field" key={field.name}>
              <span className="c-field-label">{field.label}</span>
              <input
                type="password"
                className="c-input c-input-wide"
                value={values[field.name] ?? ''}
                onChange={(e) => {
                  setError(null);
                  setValues((prev) => ({ ...prev, [field.name]: e.target.value }));
                }}
                aria-label={`${provider.display_name} ${field.label}`}
                autoComplete="off"
              />
              <span className="c-muted c-field-hint">
                Injected as <code>{field.env}</code>.
              </span>
            </label>
          ))}
          <div className="c-form-actions">
            <button type="button" className="c-btn" onClick={close}>
              Cancel
            </button>
            <button type="submit" className="c-btn c-btn-primary" disabled={setKey.isPending}>
              {setKey.isPending ? 'Saving…' : 'Save key'}
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
