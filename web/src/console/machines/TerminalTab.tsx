import { useState } from 'react';
import type { MachineSummary } from '../../api/client';
import { useMachineMutations } from '../../api/hooks';
import { Terminal } from '../../components/Terminal';
import { terminalGate } from '../machineState';

// TerminalTab is a full-height shell on the machine, with a strip of concurrent
// sessions.
//
// Every session stays mounted once opened, hidden with CSS rather than
// unmounted. Unmounting would drop its WebSocket, and reconnecting replays the
// guest's scrollback ring — so a user flipping between two sessions would watch
// each one redraw its history every time. Terminal observes its container's
// size, so a hidden pane refits itself when it comes back.
//
// Sessions are identified by an opaque id the gateway carries through; the guest
// resumes the matching PTY on reconnect (Phase 9 decision #3).
export function TerminalTab({ machine }: { machine: MachineSummary }) {
  const [sessions, setSessions] = useState<string[]>(['main']);
  const [active, setActive] = useState('main');
  const [nextId, setNextId] = useState(2);

  const gate = terminalGate(machine.state);

  const addSession = () => {
    const id = `session-${nextId}`;
    setNextId((n) => n + 1);
    setSessions((prev) => [...prev, id]);
    setActive(id);
  };

  const closeSession = (id: string) => {
    setSessions((prev) => {
      const next = prev.filter((s) => s !== id);
      if (active === id) setActive(next[next.length - 1] ?? '');
      return next;
    });
  };

  if (!gate.ok) return <BlockedTerminal machine={machine} />;

  return (
    <div className="c-terminal-page">
      <div className="c-session-bar">
        {sessions.map((id) => (
          <span key={id} className={id === active ? 'c-session c-session-active' : 'c-session'}>
            <button type="button" className="c-session-name" onClick={() => setActive(id)}>
              {id === 'main' ? 'Shell' : id.replace('session-', 'Shell ')}
            </button>
            {sessions.length > 1 && (
              <button
                type="button"
                className="c-session-close"
                onClick={() => closeSession(id)}
                aria-label={`Close ${id}`}
              >
                ×
              </button>
            )}
          </span>
        ))}
        <button type="button" className="c-btn c-session-add" onClick={addSession}>
          + New shell
        </button>
      </div>

      <div className="c-terminal-panes">
        {sessions.map((id) => (
          <div key={id} className={id === active ? 'c-terminal-pane' : 'c-terminal-pane c-hidden'}>
            <Terminal machineID={machine.id} session={id} />
          </div>
        ))}
      </div>
    </div>
  );
}

// BlockedTerminal is the explanation the WebSocket protocol cannot give. See
// terminalGate: the gateway's 409 is invisible to the browser, so without this
// the user would watch a correct refusal look like a flaky network.
function BlockedTerminal({ machine }: { machine: MachineSummary }) {
  const { start } = useMachineMutations();
  const gate = terminalGate(machine.state);

  return (
    <div className="c-panel c-empty">
      <h2 className="c-empty-title">Terminal unavailable</h2>
      <p className="c-muted">{gate.reason}</p>
      {machine.last_error && <p className="c-muted">Last error: {machine.last_error}</p>}
      {gate.offerStart && (
        <button
          type="button"
          className="c-btn c-btn-primary"
          disabled={start.isPending}
          onClick={() => start.mutate(machine.id)}
        >
          {start.isPending ? 'Starting…' : 'Start machine'}
        </button>
      )}
    </div>
  );
}
