import { useQuery } from '@tanstack/react-query';
import { api, ApiError, SessionExpiredError, type MachineState } from '../../api/client';
import { formatAge } from '../../lib/format';
import { gib } from '../../lib/format';
import { describeState } from '../machineState';
import { StateBadge } from '../ui/StateBadge';

// AdminPage is the fleet view: every machine that exists, and who owns it.
//
// Read-only by design. This is the one screen that shows a user machines that
// are not theirs, and giving it no actions keeps a mistakenly-granted
// proteos.admin role to "saw a list" rather than "could stop production".
//
// The nav hides the entry point for non-admins, but the gate that matters is
// the server's: /api/admin/overview answers 403 to everyone else regardless of
// what the client believes. This page renders that 403 as a plain explanation
// rather than bouncing, so a non-admin who follows a link learns why.
const STATE_ORDER: MachineState[] = [
  'running',
  'starting',
  'provisioning',
  'requested',
  'stopping',
  'hibernating',
  'stopped',
  'error',
];

export function AdminPage() {
  const overview = useQuery({
    queryKey: ['admin-overview'],
    queryFn: api.adminOverview,
    refetchInterval: 15_000,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      if (error instanceof ApiError) return false;
      return failureCount < 2;
    },
  });

  const forbidden = overview.error instanceof ApiError && overview.error.status === 403;

  return (
    <div className="c-page">
      <header className="c-page-header">
        <h1 className="c-page-title">Admin</h1>
        <p className="c-muted c-page-sub">
          Fleet-wide, read-only. Requires the <code>proteos.admin</code> role.
        </p>
      </header>

      {forbidden && (
        <div className="c-panel">
          <p className="c-muted">
            Your account does not hold the <code>proteos.admin</code> role, so there is nothing to
            show here.
          </p>
        </div>
      )}

      {overview.isLoading && <p className="c-muted">Loading…</p>}

      {overview.isError && !forbidden && (
        <p className="c-banner c-banner-error" role="alert">
          Could not load the fleet overview.
        </p>
      )}

      {overview.data && (
        <>
          <div className="c-stats">
            <Stat label="Machines" value={overview.data.totals.machines} />
            <Stat label="Running" value={overview.data.totals.running} emphasis />
            <Stat label="Users" value={overview.data.totals.users} />
            <Stat label="Users with machines" value={overview.data.totals.users_with_machines} />
          </div>

          <div className="c-panel">
            <h2 className="c-section-title">By state</h2>
            {/* Raw state names here, not StateBadge. The badge collapses the
                eight API states into four labels, which is right on a machine
                row and destroys a histogram — it renders "Pending 0, Pending 1,
                Pending 0" with no way to tell which is which. An admin wants
                the distinction: three machines stuck in `requested` is a
                different alarm from three `starting`. */}
            <div className="c-state-chips">
              {STATE_ORDER.map((state) => {
                const count = overview.data.totals.by_state[state] ?? 0;
                return (
                  <span
                    className={count > 0 ? 'c-state-chip' : 'c-state-chip c-state-chip-zero'}
                    key={state}
                  >
                    <span
                      className={`c-badge-dot c-state-dot-${describeState(state).tone}`}
                      aria-hidden
                    />
                    <span className="c-state-name">{state}</span>
                    <span className="c-state-count">{count}</span>
                  </span>
                );
              })}
            </div>
          </div>

          {overview.data.truncated && (
            <p className="c-banner c-banner-warn" role="status">
              The table below is a partial list. The totals above are counted separately and stay
              exact.
            </p>
          )}

          <table className="c-table">
            <thead>
              <tr>
                <th>Machine</th>
                <th>State</th>
                <th>Owner</th>
                <th>Size</th>
                <th>Host</th>
                <th>Age</th>
              </tr>
            </thead>
            <tbody>
              {overview.data.machines.map((m) => (
                <tr key={m.id} className="c-row">
                  <td>
                    <span className="c-machine-name">{m.name}</span>
                  </td>
                  <td>
                    <StateBadge state={m.state} />
                    {m.last_error && (
                      <span className="c-row-error" title={m.last_error}>
                        {m.last_error}
                      </span>
                    )}
                  </td>
                  <td>
                    {m.owner.login}
                    <span className="c-muted c-owner-email"> {m.owner.email}</span>
                  </td>
                  <td className="c-nowrap">
                    {m.resource_spec.vcpus} vCPU · {gib(m.resource_spec.mem_mib)}
                  </td>
                  <td className="c-nowrap">
                    {m.host ?? <span className="c-muted">unscheduled</span>}
                  </td>
                  <td className="c-nowrap" title={m.created_at}>
                    {formatAge(Date.now() - Date.parse(m.created_at))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </div>
  );
}

function Stat({ label, value, emphasis }: { label: string; value: number; emphasis?: boolean }) {
  return (
    <div className={emphasis ? 'c-stat c-stat-emphasis' : 'c-stat'}>
      <span className="c-stat-value">{value}</span>
      <span className="c-muted c-stat-label">{label}</span>
    </div>
  );
}
