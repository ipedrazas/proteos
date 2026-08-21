import { useState } from 'react';
import { useSSHKey, useSSHKeyMutations } from '../../api/hooks';
import { describeApiError } from '../errors';
import { CodeBlock } from '../ui/CodeBlock';
import { Dialog } from '../ui/Dialog';

// GitKeySection manages the single key your MACHINES use to push to git hosts.
//
// It is the mirror image of the login keys above and the page keeps them apart
// deliberately, because conflating them is the easiest mistake on this screen:
//
//   login keys   inbound   you -> machine   you hold the private half
//   git key      outbound  machine -> git   the server holds the private half
//
// That is also why this one IS server-generated: the machine is the party doing
// the authenticating, so the private half belongs on the server and never needs
// to reach the user. The public half is meant to be copied out, to GitHub or
// another host's deploy-key settings.
export function GitKeySection() {
  const key = useSSHKey();
  const { generate, remove } = useSSHKeyMutations();
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const present = key.data?.present ?? false;

  const doGenerate = () => {
    setError(null);
    generate.mutate(undefined, {
      onError: (e) => setError(describeApiError(e, 'Could not generate a key.')),
    });
  };

  return (
    <div className="c-panel">
      <h2 className="c-section-title">Machine git key (outbound)</h2>
      <p className="c-muted">
        A key your machines use to push to git hosts. This is the opposite direction to the keys
        above: here the machine is what authenticates, so ProteOS holds the private half and you
        copy the public half to GitHub or another host. It gives nobody access to your machines.
      </p>

      {key.isLoading && <p className="c-muted">Loading…</p>}

      {!key.isLoading && !present && (
        <>
          <p className="c-muted c-keys-empty">
            No key yet. Generate one if you want to push over SSH rather than HTTPS.
          </p>
          <button
            type="button"
            className="c-btn"
            onClick={doGenerate}
            disabled={generate.isPending}
          >
            {generate.isPending ? 'Generating…' : 'Generate key'}
          </button>
        </>
      )}

      {present && key.data?.public_key && (
        <>
          <p className="c-field-label">Public key — add this to your git host</p>
          <CodeBlock text={key.data.public_key} label="Copy public key" />
          {key.data.fingerprint && (
            <p className="c-muted">
              Fingerprint: <span className="c-fingerprint">{key.data.fingerprint}</span>
            </p>
          )}
          <div className="c-form-actions c-form-actions-left">
            <button
              type="button"
              className="c-btn"
              onClick={doGenerate}
              disabled={generate.isPending}
            >
              {generate.isPending ? 'Replacing…' : 'Replace key'}
            </button>
            <button
              type="button"
              className="c-btn c-btn-danger"
              onClick={() => setConfirmDelete(true)}
            >
              Delete key
            </button>
          </div>
          <p className="c-muted c-field-hint">
            Replacing generates a new pair — the old public key stops working the moment it is
            replaced, so update your git host afterwards.
          </p>
        </>
      )}

      {error && (
        <p className="c-banner c-banner-error" role="alert">
          {error}
        </p>
      )}

      {confirmDelete && (
        <Dialog
          title="Delete git key"
          onClose={() => setConfirmDelete(false)}
          dismissable={!remove.isPending}
        >
          <p className="c-dialog-warning">
            Delete the machine git key? Pushes over SSH from your machines will fail until you
            generate a new one and add it to your git host.
          </p>
          <div className="c-dialog-actions">
            <button type="button" className="c-btn" onClick={() => setConfirmDelete(false)}>
              Cancel
            </button>
            <button
              type="button"
              className="c-btn c-btn-danger"
              disabled={remove.isPending}
              onClick={() =>
                remove.mutate(undefined, {
                  onSuccess: () => setConfirmDelete(false),
                  onError: (e) => {
                    setError(describeApiError(e, 'Could not delete the key.'));
                    setConfirmDelete(false);
                  },
                })
              }
            >
              {remove.isPending ? 'Deleting…' : 'Delete'}
            </button>
          </div>
        </Dialog>
      )}
    </div>
  );
}
