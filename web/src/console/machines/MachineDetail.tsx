import { useState } from 'react';
import { Link, Navigate, useParams } from 'react-router';
import type { MachineSummary, Me } from '../../api/client';
import { useMachineMutations, useMachines } from '../../api/hooks';
import { describeApiError } from '../errors';
import { machineActions } from '../machineState';
import { isMachineTab, MACHINES, machineOverviewPath, type MachineTab } from '../paths';
import { CopyButton } from '../ui/CopyButton';
import { StateBadge } from '../ui/StateBadge';
import { ConnectTab } from './ConnectTab';
import { MachineTabs } from './MachineTabs';
import { OverviewTab } from './OverviewTab';
import { TerminalTab } from './TerminalTab';
import { TerminateDialog } from './TerminateDialog';

// MachineDetail is one machine's page: an identity header with its lifecycle
// actions, over the Overview content.
//
// It reads the machine out of the ['machines'] cache rather than fetching
// GET /api/machines/{id}. The cache is seeded from /api/me and kept live by the
// SSE stream ConsoleApp subscribes to, so reading it means this page updates
// with the fleet for free — a machine started here flips to Running without
// this component knowing anything about it. A per-machine fetch would be a
// second source of truth that could disagree with the table one click away.
export function MachineDetail({ me }: { me: Me }) {
  const { id, tab } = useParams();
  const machines = useMachines(me.machines).data ?? [];
  const machine = machines.find((m) => m.id === id) ?? null;

  // An unknown tab in the URL is a typo or a stale bookmark from before a tab
  // was renamed. Land on Overview rather than rendering an empty body — the one
  // tab that is always meaningful.
  if (id && !isMachineTab(tab)) {
    return <Navigate to={machineOverviewPath(id)} replace />;
  }

  if (!machine) {
    return (
      <div className="c-page">
        <header className="c-page-header">
          <h1 className="c-page-title">Machine not found</h1>
        </header>
        <div className="c-panel">
          <p className="c-muted">
            This machine does not exist, or it has been terminated. Terminating one from another tab
            removes it here too.
          </p>
          <Link className="c-btn" to={MACHINES}>
            Back to machines
          </Link>
        </div>
      </div>
    );
  }

  return <MachinePage machine={machine} tab={tab as MachineTab} />;
}

function MachinePage({ machine, tab }: { machine: MachineSummary; tab: MachineTab }) {
  const { start, stop } = useMachineMutations();
  const [error, setError] = useState<string | null>(null);
  const [terminating, setTerminating] = useState(false);

  const actions = machineActions(machine.state);

  // A single machine calls the mutation directly rather than going through
  // bulk.ts. The fan-out exists for a selection; what actually matters is that
  // both surfaces derive enablement from machineActions and phrase failures
  // through describeApiError — those are the parts that could silently diverge.
  const run = (kind: 'start' | 'stop') => {
    setError(null);
    const mutation = kind === 'start' ? start : stop;
    mutation.mutate(machine.id, {
      onError: (e) => setError(describeApiError(e, `Could not ${kind} the machine.`)),
    });
  };

  const busy = start.isPending || stop.isPending;

  return (
    <div className="c-page">
      <header className="c-page-header">
        <Link className="c-breadcrumb" to={MACHINES}>
          ← Machines
        </Link>
        <div className="c-page-header-row">
          <div>
            <MachineName machine={machine} />
            <p className="c-page-sub c-muted">
              <StateBadge state={machine.state} />
              <span className="c-id">{machine.id}</span>
              <CopyButton value={machine.id} label="Copy machine ID" />
            </p>
          </div>
          <div className="c-page-header-actions">
            <button
              type="button"
              className="c-btn"
              disabled={!actions.start || busy}
              onClick={() => run('start')}
            >
              Start
            </button>
            <button
              type="button"
              className="c-btn"
              disabled={!actions.stop || busy}
              onClick={() => run('stop')}
            >
              Stop
            </button>
            <button
              type="button"
              className="c-btn c-btn-danger"
              onClick={() => setTerminating(true)}
            >
              Terminate
            </button>
          </div>
        </div>
      </header>

      {error && (
        <p className="c-banner c-banner-error" role="alert">
          {error}
        </p>
      )}

      <MachineTabs id={machine.id} active={tab} />

      {tab === 'overview' && <OverviewTab machine={machine} />}
      {tab === 'connect' && <ConnectTab machine={machine} />}
      {tab === 'terminal' && <TerminalTab machine={machine} />}

      {terminating && (
        <TerminateDialog machines={[machine]} onClose={() => setTerminating(false)} />
      )}
    </div>
  );
}

// MachineName is the inline rename. A machine's name is the only thing about it
// a user owns, and it is usually wrong first ("machine-4") and renamed once the
// machine has a job — so renaming lives on the name itself rather than behind a
// settings screen.
function MachineName({ machine }: { machine: MachineSummary }) {
  const { rename } = useMachineMutations();
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(machine.name);
  const [error, setError] = useState<string | null>(null);

  const open = () => {
    setDraft(machine.name);
    setError(null);
    setEditing(true);
  };

  const commit = () => {
    const next = draft.trim();
    if (next === '' || next === machine.name) {
      setEditing(false);
      return;
    }
    rename.mutate(
      { id: machine.id, name: next },
      {
        onSuccess: () => setEditing(false),
        onError: (e) => setError(describeApiError(e, 'Could not rename the machine.')),
      },
    );
  };

  if (!editing) {
    // The button sits beside the heading, not inside it: an h1 is the page's
    // accessible name, and a control nested in it becomes part of that name
    // ("api-dev Rename") for anyone reading the page by its structure.
    return (
      <div className="c-name-row">
        <h1 className="c-page-title">{machine.name}</h1>
        <button type="button" className="c-btn c-btn-link c-rename" onClick={open}>
          Rename
        </button>
      </div>
    );
  }

  return (
    <div className="c-rename-form">
      <input
        type="text"
        className="c-input"
        value={draft}
        autoFocus
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') commit();
          if (e.key === 'Escape') setEditing(false);
        }}
        aria-label="Machine name"
      />
      <button
        type="button"
        className="c-btn c-btn-primary"
        onClick={commit}
        disabled={rename.isPending}
      >
        {rename.isPending ? 'Saving…' : 'Save'}
      </button>
      <button type="button" className="c-btn" onClick={() => setEditing(false)}>
        Cancel
      </button>
      {error && <span className="c-rename-error">{error}</span>}
    </div>
  );
}
