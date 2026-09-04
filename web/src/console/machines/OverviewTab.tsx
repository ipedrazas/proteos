import type { MachineSummary } from '../../api/client';
import { formatAge, formatTimestamp, gib } from '../../lib/format';

// backendLabel names the backend a machine runs on. The distinction matters to
// the reader of this panel and nowhere else in the console: it is why a machine
// has no snapshot and never will (K3).
function backendLabel(backend: MachineSummary['backend']): string {
  switch (backend) {
    case 'kubernetes':
      return 'Kubernetes pod';
    case 'firecracker':
      return 'Firecracker microVM';
    case 'dev':
      return 'Development stub';
    default:
      return '—';
  }
}

// OverviewTab is everything the control plane already knows about one machine.
//
// MachineSummary has carried these fields to the browser since Phase 4 and no
// surface has ever rendered them (plans/machine-templates-implementation.md
// made the same observation about the desktop). Hibernation in particular is
// invisible today: a "Stopped" machine with a snapshot resumes from memory
// rather than cold-booting, which is a materially different thing to press
// Start on, and until now nothing said so.
//
// K3 adds the other half of that: a machine on a Kubernetes host never
// hibernates at all, so the snapshot row is hidden rather than shown empty.
export function OverviewTab({ machine }: { machine: MachineSummary }) {
  const spec = machine.resource_spec;
  const created = Date.parse(machine.created_at);
  // A pod has no RAM to snapshot and restore, so a Kubernetes machine stops by
  // powering off and always cold boots. Showing it an empty "Snapshot" row would
  // read as "none yet" — a state it could reach — rather than "never".
  const hibernates = machine.backend !== 'kubernetes';

  return (
    <>
      {machine.last_error && (
        <div className="c-banner c-banner-error" role="alert">
          <strong>Last error:</strong> {machine.last_error}
        </div>
      )}

      <div className="c-panel">
        <h2 className="c-section-title">Configuration</h2>
        <dl className="c-defs">
          <dt>Template</dt>
          <dd>
            {machine.template_id ?? <span className="c-muted">— (created before templates)</span>}
          </dd>
          <dt>vCPUs</dt>
          <dd>{spec.vcpus}</dd>
          <dt>Memory</dt>
          <dd>{gib(spec.mem_mib)}</dd>
          <dt>Disk</dt>
          <dd>{gib(machine.disk_mib ?? spec.disk_mib)}</dd>
          <dt>Address</dt>
          <dd>{machine.guest_ip ?? <span className="c-muted">— (assigned while running)</span>}</dd>
          <dt>Backend</dt>
          <dd>{backendLabel(machine.backend)}</dd>
        </dl>
      </div>

      <div className="c-panel">
        <h2 className="c-section-title">Storage and boot</h2>
        <dl className="c-defs">
          <dt>Disk ID</dt>
          <dd>{machine.disk_id ?? <span className="c-muted">—</span>}</dd>
          <dt>Last boot</dt>
          <dd>
            {machine.boot === 'resumed' ? (
              'Resumed from snapshot'
            ) : machine.boot === 'cold' ? (
              <>
                Cold boot
                {!hibernates && (
                  <span className="c-muted"> · stopping powers this machine off</span>
                )}
              </>
            ) : (
              <span className="c-muted">— (not booted yet)</span>
            )}
          </dd>
          {hibernates && (
            <>
              <dt>Snapshot</dt>
              <dd>
                {machine.snapshot ? (
                  <>
                    {gib(Math.round(machine.snapshot.mem_bytes / (1024 * 1024)))} of memory, taken{' '}
                    {formatTimestamp(machine.snapshot.created_at)}
                    <span className="c-muted"> · Firecracker {machine.snapshot.fc_version}</span>
                  </>
                ) : (
                  <span className="c-muted">None — this machine will cold boot</span>
                )}
              </dd>
            </>
          )}
        </dl>
      </div>

      <div className="c-panel">
        <h2 className="c-section-title">History</h2>
        <dl className="c-defs">
          <dt>Created</dt>
          <dd>
            {formatTimestamp(machine.created_at)}
            {!Number.isNaN(created) && (
              <span className="c-muted"> · {formatAge(Date.now() - created)}</span>
            )}
          </dd>
          <dt>Last active</dt>
          <dd>
            {machine.last_active_at ? (
              formatTimestamp(machine.last_active_at)
            ) : (
              <span className="c-muted">Never</span>
            )}
          </dd>
        </dl>
      </div>
    </>
  );
}
