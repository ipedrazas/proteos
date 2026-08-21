import { Link } from 'react-router';
import { useTemplates } from '../../api/hooks';
import { gib } from '../../lib/format';
import { MACHINES_LAUNCH } from '../paths';

// TemplatesPage is the machine-image catalog, read-only.
//
// Read-only because the catalog is operator-authored config on the control
// plane, not user data — changing it is a redeploy. The page exists so the
// limits are inspectable before you reach the launch wizard, and so "why can I
// only give this 2 GiB?" has an answer that is not "try it and see".
export function TemplatesPage() {
  const templates = useTemplates();
  const rows = templates.data ?? [];

  return (
    <div className="c-page">
      <header className="c-page-header c-page-header-row">
        <div>
          <h1 className="c-page-title">Templates</h1>
          <p className="c-muted c-page-sub">
            The machine images you can launch from. Set by the operator — a template varies the
            language toolchain only; the platform layer is identical in every one.
          </p>
        </div>
        <div className="c-page-header-actions">
          <Link className="c-btn c-btn-primary" to={MACHINES_LAUNCH}>
            Launch machine
          </Link>
        </div>
      </header>

      {templates.isLoading && <p className="c-muted">Loading…</p>}
      {templates.isError && (
        <p className="c-banner c-banner-error" role="alert">
          Could not load the catalog.
        </p>
      )}
      {!templates.isLoading && rows.length === 0 && (
        <div className="c-panel">
          <p className="c-muted">
            No templates are configured on this deployment. The catalog lives in the control
            plane&apos;s configuration.
          </p>
        </div>
      )}

      {rows.length > 0 && (
        <table className="c-table">
          <thead>
            <tr>
              <th>Template</th>
              <th>Default size</th>
              <th>vCPU range</th>
              <th>Memory range</th>
              <th>Disk range</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((t) => (
              <tr key={t.id} className="c-row">
                <td>
                  <span className="c-machine-name">{t.label}</span>
                  <span className="c-muted c-template-row-desc">{t.description}</span>
                </td>
                <td className="c-nowrap">
                  {t.defaults.vcpus} vCPU · {gib(t.defaults.mem_mib)} · {gib(t.defaults.disk_mib)}
                </td>
                <td className="c-nowrap">
                  {t.limits.vcpus.min}–{t.limits.vcpus.max}
                </td>
                <td className="c-nowrap">
                  {gib(t.limits.mem_mib.min)}–{gib(t.limits.mem_mib.max)}
                </td>
                <td className="c-nowrap">
                  {gib(t.limits.disk_mib.min)}–{gib(t.limits.disk_mib.max)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
