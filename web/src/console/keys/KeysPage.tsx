import { useState } from 'react';
import { useMachines, useMe, useUpdateUserPrefs } from '../../api/hooks';
import type { Me } from '../../api/client';
import { describeApiError } from '../errors';
import { CodeBlock } from '../ui/CodeBlock';
import { Dialog } from '../ui/Dialog';
import { GitKeySection } from './GitKeySection';
import { LoginKeysSection } from './LoginKeysSection';

// KeysPage is everything about SSH in one place: the account-wide switch, the
// keys you log in with, the key your machines push with, and the commands.
//
// The page is organised around the one thing that makes SSH here different from
// a normal server, and which the copy states rather than assumes: a machine has
// no inbound port and no address of its own. Its sshd binds loopback only, and
// the only route to it is an authenticated tunnel through the control plane. So
// there is no host to point a client at, and the switch below is a real
// kill-switch rather than a display preference.
export function KeysPage({ me }: { me: Me }) {
  const live = useMe().data ?? me;
  const enabled = live.prefs.ssh_enabled;

  return (
    <div className="c-page">
      <header className="c-page-header">
        <h1 className="c-page-title">SSH keys</h1>
        <p className="c-muted c-page-sub">
          Log into your machines from a local terminal, and give them a key to push to git hosts
          with.
        </p>
      </header>

      <SshSwitch enabled={enabled} />
      <LoginKeysSection enabled={enabled} />
      <GitKeySection />
      <ConnectInstructions enabled={enabled} />
    </div>
  );
}

// SshSwitch is the account-wide kill switch. Turning it off is a real security
// action, not a display toggle — the control plane stops injecting
// authorized_keys into every machine AND refuses the tunnel — so the confirm
// spells out both consequences instead of asking a bare "are you sure?".
function SshSwitch({ enabled }: { enabled: boolean }) {
  const update = useUpdateUserPrefs();
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const apply = (next: boolean) => {
    setError(null);
    update.mutate(
      { ssh_enabled: next },
      {
        onSuccess: () => setConfirming(false),
        onError: (e) => {
          setError(describeApiError(e, 'Could not change the setting.'));
          setConfirming(false);
        },
      },
    );
  };

  return (
    <div className="c-panel">
      <div className="c-switch-row">
        <div>
          <h2 className="c-section-title">SSH access</h2>
          <p className="c-muted">
            Off by default — registering a key does not on its own open the door. With this off,
            your keys are removed from every machine and connections are refused.
          </p>
        </div>
        <div className="c-switch-control">
          <span className={enabled ? 'c-badge c-badge-ok' : 'c-badge'}>
            <span className="c-badge-dot" aria-hidden />
            {enabled ? 'Enabled' : 'Disabled'}
          </span>
          <button
            type="button"
            className={enabled ? 'c-btn c-btn-danger' : 'c-btn c-btn-primary'}
            disabled={update.isPending}
            onClick={() => (enabled ? setConfirming(true) : apply(true))}
          >
            {update.isPending ? 'Saving…' : enabled ? 'Turn off' : 'Turn on'}
          </button>
        </div>
      </div>

      {error && (
        <p className="c-banner c-banner-error" role="alert">
          {error}
        </p>
      )}

      {confirming && (
        <Dialog
          title="Turn off SSH access"
          onClose={() => setConfirming(false)}
          dismissable={!update.isPending}
        >
          <p className="c-dialog-warning">
            Your registered keys stay saved, but they are removed from every machine and new SSH
            connections are refused until you turn this back on. Any open session is cut.
          </p>
          <div className="c-dialog-actions">
            <button type="button" className="c-btn" onClick={() => setConfirming(false)}>
              Cancel
            </button>
            <button
              type="button"
              className="c-btn c-btn-danger"
              disabled={update.isPending}
              onClick={() => apply(false)}
            >
              {update.isPending ? 'Turning off…' : 'Turn off SSH'}
            </button>
          </div>
        </Dialog>
      )}
    </div>
  );
}

// ConnectInstructions uses a real machine id from the user's own fleet when
// there is one, so the commands are copy-and-run rather than a template with a
// placeholder to substitute.
function ConnectInstructions({ enabled }: { enabled: boolean }) {
  const machines = useMachines([]).data ?? [];
  const target = machines.find((m) => m.state === 'running') ?? machines[0];
  const id = target?.id ?? '<machine-id>';

  return (
    <div className="c-panel">
      <h2 className="c-section-title">How to connect</h2>
      <p className="c-muted">
        Machines have no public address and no inbound port, so connections go through the{' '}
        <code>proteos</code> CLI — which is also what authenticates you. Install it from Secrets →
        tokens and sign in with <code>proteos auth login</code>.
      </p>

      {!enabled && (
        <p className="c-banner c-banner-error" role="status">
          SSH is off for your account, so these commands will be refused until you turn it on above.
        </p>
      )}

      <p className="c-field-label">Open a shell</p>
      <CodeBlock text={`proteos ssh ${id}`} label="Copy ssh command" />
      {target && (
        <p className="c-muted c-field-hint">
          That is your machine <strong>{target.name}</strong>. Use <code>proteos machines ls</code>{' '}
          to see the others.
        </p>
      )}

      <p className="c-field-label">Use scp, rsync, or an IDE&apos;s Remote-SSH</p>
      <p className="c-muted">
        Add this to <code>~/.ssh/config</code>, then any SSH-based tool can address the machine by
        name.
      </p>
      <CodeBlock
        text={`Host proteos-${id}\n    User dev\n    ProxyCommand proteos ssh-proxy ${id}`}
        label="Copy ssh config"
      />

      <p className="c-muted">
        You log in as <code>dev</code>. Password login is disabled entirely — only the keys above
        are accepted. Each machine keeps its host key across stop/start, so your{' '}
        <code>known_hosts</code> entry stays valid.
      </p>
    </div>
  );
}
