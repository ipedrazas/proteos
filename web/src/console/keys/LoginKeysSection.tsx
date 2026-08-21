import { useState } from 'react';
import type { SSHLoginKey } from '../../api/client';
import { useSSHLoginKeyMutations, useSSHLoginKeys } from '../../api/hooks';
import { formatTimestamp } from '../../lib/format';
import { describeApiError } from '../errors';
import { Dialog } from '../ui/Dialog';
import { checkPublicKey, suggestLabel } from './publicKey';

// LoginKeysSection manages the keys that let you SSH INTO your machines.
//
// Note the deliberate absence of a "generate a key for me" action. A login
// key's private half must never reach the server — that is the entire security
// property of public-key auth — so the user pastes a public key they already
// hold. The outbound git key in the next section IS server-generated, because
// there the machine is the one doing the authenticating; the two are opposites
// and the page says so rather than leaving it to be inferred.
export function LoginKeysSection({ enabled }: { enabled: boolean }) {
  const keys = useSSHLoginKeys();
  const [adding, setAdding] = useState(false);
  const [revoking, setRevoking] = useState<SSHLoginKey | null>(null);

  const rows = keys.data ?? [];

  return (
    <div className="c-panel">
      <h2 className="c-section-title">Your login keys (inbound)</h2>
      <p className="c-muted">
        Public keys allowed to log into your machines as <code>dev</code>. Paste a key you already
        have — ProteOS never asks for, and never stores, a private key.
      </p>

      {keys.isError && (
        <p className="c-banner c-banner-error" role="alert">
          Could not load your keys.
        </p>
      )}

      {rows.length === 0 ? (
        <p className="c-muted c-keys-empty">
          {keys.isLoading
            ? 'Loading…'
            : 'No keys registered. Without one there is nothing to log in with.'}
        </p>
      ) : (
        <table className="c-table c-keys-table">
          <thead>
            <tr>
              <th>Label</th>
              <th>Fingerprint</th>
              <th>Added</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {rows.map((k) => (
              <tr key={k.id} className="c-row">
                <td>{k.label}</td>
                <td className="c-fingerprint">{k.fingerprint}</td>
                <td className="c-nowrap">{formatTimestamp(k.created_at)}</td>
                <td className="c-col-action">
                  <button
                    type="button"
                    className="c-btn c-btn-danger"
                    onClick={() => setRevoking(k)}
                  >
                    Revoke
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {!enabled && rows.length > 0 && (
        <p className="c-muted c-keys-note">
          These keys are saved but not in use — SSH access is off for your account.
        </p>
      )}

      {adding ? (
        <AddKeyForm onDone={() => setAdding(false)} />
      ) : (
        <button type="button" className="c-btn c-keys-add" onClick={() => setAdding(true)}>
          Add key
        </button>
      )}

      {revoking && <RevokeDialog sshKey={revoking} onClose={() => setRevoking(null)} />}
    </div>
  );
}

function AddKeyForm({ onDone }: { onDone: () => void }) {
  const { add } = useSSHLoginKeyMutations();
  const [publicKey, setPublicKey] = useState('');
  const [label, setLabel] = useState('');
  const [labelTouched, setLabelTouched] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The label defaults to the key's own comment (ssh-keygen writes user@host),
  // so the common case needs no typing — until the user types, at which point
  // their choice sticks.
  const onKeyChange = (value: string) => {
    setPublicKey(value);
    setError(null);
    if (labelTouched) return;
    const parsed = checkPublicKey(value);
    if (parsed.ok) setLabel(suggestLabel(parsed.comment));
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const parsed = checkPublicKey(publicKey);
    if (!parsed.ok) {
      setError(parsed.error);
      return;
    }
    add.mutate(
      { label: label.trim() || parsed.comment || parsed.type, publicKey: parsed.key },
      {
        onSuccess: onDone,
        onError: (err) => setError(describeApiError(err, 'Could not add the key.')),
      },
    );
  };

  return (
    <form className="c-key-form" onSubmit={submit}>
      <label className="c-field">
        <span className="c-field-label">Public key</span>
        <textarea
          className="c-textarea"
          rows={3}
          value={publicKey}
          onChange={(e) => onKeyChange(e.target.value)}
          placeholder="ssh-ed25519 AAAA… ada@laptop"
          aria-label="Public key"
          autoFocus
        />
        <span className="c-muted c-field-hint">
          The contents of your <code>.pub</code> file — for example{' '}
          <code>cat ~/.ssh/id_ed25519.pub</code>.
        </span>
      </label>

      <label className="c-field">
        <span className="c-field-label">Label</span>
        <input
          type="text"
          className="c-input"
          value={label}
          onChange={(e) => {
            setLabelTouched(true);
            setLabel(e.target.value);
          }}
          placeholder="laptop"
          aria-label="Key label"
        />
      </label>

      {error && (
        <p className="c-banner c-banner-error" role="alert">
          {error}
        </p>
      )}

      <div className="c-form-actions">
        <button type="button" className="c-btn" onClick={onDone}>
          Cancel
        </button>
        <button type="submit" className="c-btn c-btn-primary" disabled={add.isPending}>
          {add.isPending ? 'Adding…' : 'Add key'}
        </button>
      </div>
    </form>
  );
}

function RevokeDialog({ sshKey, onClose }: { sshKey: SSHLoginKey; onClose: () => void }) {
  const { revoke } = useSSHLoginKeyMutations();
  const [error, setError] = useState<string | null>(null);

  const run = () =>
    revoke.mutate(sshKey.id, {
      onSuccess: onClose,
      onError: (e) => setError(describeApiError(e, 'Could not revoke the key.')),
    });

  return (
    <Dialog title="Revoke key" onClose={onClose} dismissable={!revoke.isPending}>
      <p className="c-dialog-warning">
        Revoke <strong>{sshKey.label}</strong>? It is removed from every machine, and anyone holding
        the matching private key loses access immediately.
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
          onClick={run}
          disabled={revoke.isPending}
        >
          {revoke.isPending ? 'Revoking…' : 'Revoke'}
        </button>
      </div>
    </Dialog>
  );
}
