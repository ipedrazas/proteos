import type { MachineSummary } from '../../api/client';
import { formatAge, formatTimestamp, gib } from '../../lib/format';

// OverviewTab is everything the control plane already knows about one machine.
//
// MachineSummary has carried these fields to the browser since Phase 4 and no
// surface has ever rendered them (plans/machine-templates-implementation.md
// made the same observation about the desktop). Hibernation in particular is
// invisible today: a "Stopped" machine with a snapshot resumes from memory
// rather than cold-booting, which is a materially different thing to press
// Start on, and until now nothing said so.
export function OverviewTab({ machine }: { machine: MachineSummary }) {
  const spec = machine.resource_spec;
  const created = Date.parse(machine.created_at);

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
              'Cold boot'
            ) : (
              <span className="c-muted">— (not booted yet)</span>
            )}
          </dd>
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
