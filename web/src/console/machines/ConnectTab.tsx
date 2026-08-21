import { useState } from 'react';
import { Link } from 'react-router';
import { api, type MachineSummary } from '../../api/client';
import { useMe, useSSHLoginKeys } from '../../api/hooks';
import { describeApiError } from '../errors';
import { terminalGate } from '../machineState';
import { machineTabPath } from '../paths';
import { CodeBlock } from '../ui/CodeBlock';

// ConnectTab is the "how do I get into this machine" page: three routes in,
// each stated with what it needs.
//
// The thing worth being explicit about, and the reason this page reads
// differently from a cloud VM's connect tab: a ProteOS machine has no inbound
// port and no address of its own. The guest's sshd binds loopback only, and the
// only path to it is an authenticated tunnel through the control plane. So
// there is no host to point a client at — every route in goes through ProteOS,
// which is also what authenticates you.
export function ConnectTab({ machine }: { machine: MachineSummary }) {
  const gate = terminalGate(machine.state);

  return (
    <>
      <div className="c-panel">
        <h2 className="c-section-title">Web terminal</h2>
        <p className="c-muted">
          A shell in the browser, over the same authenticated tunnel. Nothing to install.
        </p>
        {gate.ok ? (
          <Link
            className="c-btn c-btn-primary c-connect-action"
            to={machineTabPath(machine.id, 'terminal')}
          >
            Open terminal
          </Link>
        ) : (
          <p className="c-muted c-connect-blocked">{gate.reason}</p>
        )}
      </div>

      <EditorSection machine={machine} />
      <SshSection machine={machine} />
    </>
  );
}

// EditorSection mints a one-shot web session and opens code-server in a new tab.
//
// A new tab rather than an iframe (which is what the desktop shell does): the
// editor is a full application with its own keyboard handling, and the console
// is a control panel, not a window manager. The token is single-use and lives
// ≤60s, so it is minted per click and never held.
function EditorSection({ machine }: { machine: MachineSummary }) {
  const [error, setError] = useState<string | null>(null);
  const [opening, setOpening] = useState(false);
  const running = machine.state === 'running';

  const open = async () => {
    setError(null);
    setOpening(true);
    try {
      const session = await api.webSession(machine.id);
      window.open(session.url, '_blank', 'noopener');
    } catch (e) {
      setError(describeApiError(e, 'Could not open the editor.'));
    } finally {
      setOpening(false);
    }
  };

  return (
    <div className="c-panel">
      <h2 className="c-section-title">Editor</h2>
      <p className="c-muted">
        VS Code in the browser, opened on this machine&apos;s workspace. Runs on the machine&apos;s
        own subdomain.
      </p>
      {running ? (
        <button
          type="button"
          className="c-btn c-connect-action"
          onClick={() => void open()}
          disabled={opening}
        >
          {opening ? 'Opening…' : 'Open editor'}
        </button>
      ) : (
        <p className="c-muted c-connect-blocked">The editor needs the machine to be running.</p>
      )}
      {error && (
        <p className="c-banner c-banner-error" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}

// SshSection is the local-client route. It is gated on two things that are easy
// to get wrong separately: the account-wide SSH switch, and having registered a
// public key. Both are account-level, so this page states them and links to the
// one place they are managed rather than duplicating the controls.
function SshSection({ machine }: { machine: MachineSummary }) {
  const me = useMe();
  const keys = useSSHLoginKeys();
  const enabled = me.data?.prefs.ssh_enabled ?? false;
  const hasKeys = (keys.data?.length ?? 0) > 0;

  return (
    <div className="c-panel">
      <h2 className="c-section-title">SSH client</h2>
      <p className="c-muted">
        This machine has no inbound port and no address of its own — its SSH server listens on
        loopback only, and the sole route to it is an authenticated tunnel. So connections go
        through the <code>proteos</code> CLI, which is also what authenticates you.
      </p>

      {!enabled && (
        <p className="c-banner c-banner-error" role="status">
          SSH is off for your account, so these commands will be refused.{' '}
          <Link to="/console/keys">Turn it on under SSH keys.</Link>
        </p>
      )}
      {enabled && !hasKeys && (
        <p className="c-banner c-banner-error" role="status">
          You have not registered a public key, so there is nothing to log in with.{' '}
          <Link to="/console/keys">Add one under SSH keys.</Link>
        </p>
      )}

      <p className="c-field-label">Open a shell</p>
      <CodeBlock text={`proteos ssh ${machine.id}`} label="Copy ssh command" />

      <p className="c-field-label">Run a single command</p>
      <CodeBlock text={`proteos ssh ${machine.id} -- uname -a`} label="Copy command" />

      <p className="c-field-label">Use scp, rsync, or an IDE&apos;s Remote-SSH</p>
      <p className="c-muted">
        Add this to <code>~/.ssh/config</code> and any SSH-based tool can address the machine by
        name — <code>ssh proteos-{machine.id.slice(0, 8)}…</code>, or VS Code Remote-SSH.
      </p>
      <CodeBlock
        text={`Host proteos-${machine.id}\n    User dev\n    ProxyCommand proteos ssh-proxy ${machine.id}`}
        label="Copy ssh config"
      />

      <p className="c-muted">
        You log in as <code>dev</code>. Password login is disabled — only registered keys are
        accepted. The machine keeps its host key across stop/start, so <code>known_hosts</code>{' '}
        stays valid.
      </p>
    </div>
  );
}
