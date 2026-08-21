import { useMemo, useState } from 'react';
import { Link } from 'react-router';
import type { Me, MachineSummary } from '../../api/client';
import { useMachineMutations, useMachines } from '../../api/hooks';
import { formatAge, gib } from '../../lib/format';
import { failures, runBulk } from '../bulk';
import { describeApiError } from '../errors';
import { machineActions } from '../machineState';
import { filterMachines, STATE_FILTERS, type StateFilter } from '../machinesFilter';
import { MACHINES_LAUNCH } from '../nav';
import { StateBadge } from '../ui/StateBadge';
import { TerminateDialog } from './TerminateDialog';

// RowMark is the transient per-row state during a bulk action. Success is not a
// mark — the row's own state badge changes, which is the real feedback; a green
// tick alongside a badge that already says "Running" is noise.
type RowMark = 'working' | 'failed';

// MachinesList is the console's main surface: every machine the user owns, in
// one dense table, with lifecycle actions driven by selection.
//
// Selection-driven rather than per-row buttons, which is the AWS shape and has a
// practical payoff: acting on one machine and acting on eight are the same code
// path, so there is no single-machine flow that can drift from the bulk one.
//
// The table never polls. useMachineEvents (mounted once in ConsoleApp) writes
// SSE state straight into the ['machines'] query cache, so a machine started
// here moves Pending → Running on its own.
export function MachinesList({ me }: { me: Me }) {
  const machinesQuery = useMachines(me.machines);
  // Memoised, not `query.data ?? []` inline: the fallback allocates a fresh
  // array every render, which would give the filter and selection memos below a
  // changing dependency and defeat them entirely.
  const machines = useMemo(() => machinesQuery.data ?? [], [machinesQuery.data]);
  const { start, stop } = useMachineMutations();

  const [query, setQuery] = useState('');
  const [stateFilter, setStateFilter] = useState<StateFilter>('all');
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const [marks, setMarks] = useState<ReadonlyMap<string, RowMark>>(new Map());
  const [error, setError] = useState<string | null>(null);
  const [terminating, setTerminating] = useState<MachineSummary[] | null>(null);

  const visible = useMemo(
    () => filterMachines(machines, query, stateFilter),
    [machines, query, stateFilter],
  );

  // Derived from the live list, so ids of machines that have since been
  // destroyed drop out on their own rather than lingering in the selection.
  const selected = useMemo(
    () => machines.filter((m) => selectedIds.has(m.id)),
    [machines, selectedIds],
  );

  const toggle = (id: string) =>
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  // Select-all acts on what is on screen, not the whole fleet — otherwise
  // filtering to "error" and hitting the header checkbox would select machines
  // the user cannot see, which is how people terminate the wrong thing.
  const allVisibleSelected = visible.length > 0 && visible.every((m) => selectedIds.has(m.id));
  const toggleAllVisible = () =>
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (allVisibleSelected) visible.forEach((m) => next.delete(m.id));
      else visible.forEach((m) => next.add(m.id));
      return next;
    });

  // Start and Stop apply to the eligible SUBSET of the selection rather than
  // requiring the whole selection to agree: select everything and hit Start and
  // the stopped ones start, which is both what EC2 does and what people mean.
  // The cost is ambiguity about how many machines a click will touch, so the
  // button carries the count whenever it is acting on less than the selection.
  //
  // Terminate deliberately does NOT work this way. It applies to everything
  // selected, and the dialog names every machine before anything happens —
  // a destructive action should never quietly act on a subset.
  const startable = selected.filter((m) => machineActions(m.state).start);
  const stoppable = selected.filter((m) => machineActions(m.state).stop);
  const subsetLabel = (verb: string, targets: MachineSummary[]) =>
    targets.length > 0 && targets.length !== selected.length ? `${verb} (${targets.length})` : verb;

  const runLifecycle = async (kind: 'start' | 'stop') => {
    const targets = kind === 'start' ? startable : stoppable;
    if (targets.length === 0) return;
    setError(null);
    setMarks(new Map(targets.map((m) => [m.id, 'working' as RowMark])));

    const mutation = kind === 'start' ? start : stop;
    // Each row clears itself as its own call lands, rather than the whole
    // selection unfreezing at the end — with a slow machine in the set that
    // difference is the whole feel of the table.
    const outcomes = await runBulk(targets, async (m) => {
      try {
        await mutation.mutateAsync(m.id);
        setMarks((prev) => {
          const next = new Map(prev);
          next.delete(m.id);
          return next;
        });
      } catch (e) {
        setMarks((prev) => new Map(prev).set(m.id, 'failed'));
        throw e;
      }
    });

    const bad = failures(outcomes);
    if (bad.length > 0) {
      const detail = describeApiError(bad[0].error, `Could not ${kind} the machine.`);
      setError(
        bad.length === targets.length
          ? detail
          : `${bad.length} of ${targets.length} could not ${kind}. ${detail}`,
      );
    }
  };

  const atLimit = machines.length >= me.machine_limit;
  const filtered = query.trim() !== '' || stateFilter !== 'all';

  return (
    <div className="c-page">
      <header className="c-page-header c-page-header-row">
        <div>
          <h1 className="c-page-title">Machines</h1>
          <p className="c-muted c-page-sub">
            {machines.length} of {me.machine_limit} used
          </p>
        </div>
        <div className="c-page-header-actions">
          <button
            type="button"
            className="c-btn"
            onClick={() => void machinesQuery.refetch()}
            disabled={machinesQuery.isFetching}
          >
            {machinesQuery.isFetching ? 'Refreshing…' : 'Refresh'}
          </button>
          <Link
            className={atLimit ? 'c-btn c-btn-primary c-btn-disabled' : 'c-btn c-btn-primary'}
            to={MACHINES_LAUNCH}
            aria-disabled={atLimit || undefined}
            title={atLimit ? 'You have reached your machine limit.' : undefined}
            onClick={(e) => {
              if (atLimit) e.preventDefault();
            }}
          >
            Launch machine
          </Link>
        </div>
      </header>

      {machines.length === 0 ? (
        <EmptyFleet />
      ) : (
        <>
          <div className="c-toolbar">
            <input
              type="search"
              className="c-input"
              placeholder="Filter by name, ID, or template"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              aria-label="Filter machines"
            />
            <select
              className="c-select"
              value={stateFilter}
              onChange={(e) => setStateFilter(e.target.value as StateFilter)}
              aria-label="Filter by state"
            >
              {STATE_FILTERS.map((f) => (
                <option key={f.value} value={f.value}>
                  {f.label}
                </option>
              ))}
            </select>

            <div className="c-toolbar-spacer" />

            <span className="c-muted c-selection-count">
              {selected.length > 0 ? `${selected.length} selected` : ''}
            </span>
            <button
              type="button"
              className="c-btn"
              disabled={startable.length === 0}
              onClick={() => void runLifecycle('start')}
            >
              {subsetLabel('Start', startable)}
            </button>
            <button
              type="button"
              className="c-btn"
              disabled={stoppable.length === 0}
              onClick={() => void runLifecycle('stop')}
            >
              {subsetLabel('Stop', stoppable)}
            </button>
            <button
              type="button"
              className="c-btn c-btn-danger"
              disabled={selected.length === 0}
              onClick={() => setTerminating(selected)}
            >
              Terminate
            </button>
          </div>

          {error && (
            <p className="c-banner c-banner-error" role="alert">
              {error}
            </p>
          )}

          {visible.length === 0 ? (
            <div className="c-panel">
              <p className="c-muted">No machines match these filters.</p>
              <button
                type="button"
                className="c-btn c-btn-link"
                onClick={() => {
                  setQuery('');
                  setStateFilter('all');
                }}
              >
                Clear filters
              </button>
            </div>
          ) : (
            <table className="c-table">
              <thead>
                <tr>
                  <th className="c-col-check">
                    <input
                      type="checkbox"
                      checked={allVisibleSelected}
                      onChange={toggleAllVisible}
                      aria-label={
                        allVisibleSelected ? 'Deselect all machines' : 'Select all machines'
                      }
                    />
                  </th>
                  <th>Name</th>
                  <th>State</th>
                  <th>Size</th>
                  <th>Disk</th>
                  <th>Template</th>
                  <th>Age</th>
                </tr>
              </thead>
              <tbody>
                {visible.map((m) => (
                  <MachineRow
                    key={m.id}
                    machine={m}
                    selected={selectedIds.has(m.id)}
                    mark={marks.get(m.id)}
                    onToggle={() => toggle(m.id)}
                  />
                ))}
              </tbody>
            </table>
          )}

          {filtered && visible.length > 0 && (
            <p className="c-muted c-table-note">
              Showing {visible.length} of {machines.length} machines.
            </p>
          )}
        </>
      )}

      {terminating && (
        <TerminateDialog machines={terminating} onClose={() => setTerminating(null)} />
      )}
    </div>
  );
}

function MachineRow({
  machine,
  selected,
  mark,
  onToggle,
}: {
  machine: MachineSummary;
  selected: boolean;
  mark: RowMark | undefined;
  onToggle: () => void;
}) {
  const spec = machine.resource_spec;
  const created = Date.parse(machine.created_at);
  return (
    <tr className={selected ? 'c-row c-row-selected' : 'c-row'}>
      <td className="c-col-check">
        <input
          type="checkbox"
          checked={selected}
          onChange={onToggle}
          aria-label={`Select ${machine.name}`}
        />
      </td>
      <td>
        <span className="c-machine-name">{machine.name}</span>
        {mark === 'working' && <span className="c-row-mark c-muted"> working…</span>}
        {mark === 'failed' && <span className="c-row-mark c-row-mark-failed"> failed</span>}
      </td>
      <td>
        <StateBadge state={machine.state} />
        {machine.last_error && (
          <span className="c-row-error" title={machine.last_error}>
            {machine.last_error}
          </span>
        )}
      </td>
      <td className="c-nowrap">
        {spec.vcpus} vCPU · {gib(spec.mem_mib)}
      </td>
      <td className="c-nowrap">{gib(machine.disk_mib ?? spec.disk_mib)}</td>
      <td>{machine.template_id ?? <span className="c-muted">—</span>}</td>
      <td className="c-nowrap" title={machine.created_at}>
        {Number.isNaN(created) ? '—' : formatAge(Date.now() - created)}
      </td>
    </tr>
  );
}

function EmptyFleet() {
  return (
    <div className="c-panel c-empty">
      <h2 className="c-empty-title">No machines yet</h2>
      <p className="c-muted">
        A machine is an isolated Linux VM with your tools, credentials, and coding agents already
        set up. Launch one to get a terminal, an editor, and somewhere to clone your repositories.
      </p>
      <Link className="c-btn c-btn-primary" to={MACHINES_LAUNCH}>
        Launch machine
      </Link>
    </div>
  );
}
