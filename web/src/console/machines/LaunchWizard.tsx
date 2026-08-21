import { useMemo, useState } from 'react';
import { Link, useNavigate } from 'react-router';
import type { MachineTemplate, Me } from '../../api/client';
import { useMachineMutations, useMachines, useTemplates } from '../../api/hooks';
import { gib } from '../../lib/format';
import { describeApiError } from '../errors';
import { MACHINES, machineOverviewPath } from '../paths';
import { clampResources, defaultResources, WIZARD_STEPS, type Resources } from './wizard';

// LaunchWizard is the full-page create flow: name, template, size, review.
//
// A page rather than a modal, which is the EC2 shape and earns its keep here —
// the size step needs room for three bounded controls and their live limits,
// and the review step is worth reading rather than squinting at.
//
// The wizard never lets a value out of the template's caps: every change is
// clamped, and switching template re-clamps whatever was already chosen. The
// control plane validates independently and answers invalid_resources, but a
// form that can produce a request the server must reject is a form that will
// eventually show someone a server error for a slider they were allowed to drag.
export function LaunchWizard({ me }: { me: Me }) {
  const navigate = useNavigate();
  const templates = useTemplates();
  const { create } = useMachineMutations();
  const machines = useMachines(me.machines).data ?? [];

  const [stepIndex, setStepIndex] = useState(0);
  const [name, setName] = useState('');
  const [templateId, setTemplateId] = useState<string | null>(null);
  const [resources, setResources] = useState<Resources | null>(null);

  const catalog = useMemo(() => templates.data ?? [], [templates.data]);
  const selected = catalog.find((t) => t.id === templateId) ?? null;

  const step = WIZARD_STEPS[stepIndex];
  const atLimit = machines.length >= me.machine_limit;

  // Choosing a template resets the size to its defaults — the template is the
  // starting point and overrides layer on top, so carrying the previous
  // template's numbers across would silently misrepresent what was chosen.
  const chooseTemplate = (t: MachineTemplate) => {
    setTemplateId(t.id);
    setResources(defaultResources(t));
  };

  const setResource = (key: keyof Resources, value: number) => {
    if (!selected) return;
    setResources((prev) =>
      clampResources({ ...(prev ?? defaultResources(selected)), [key]: value }, selected),
    );
  };

  const canAdvance =
    step.key === 'template' ? selected !== null : step.key === 'size' ? resources !== null : true;

  const submit = () => {
    if (!selected || !resources) return;
    create.mutate(
      {
        name: name.trim() || undefined,
        template_id: selected.id,
        // Clamped once more at the boundary: the state should already be
        // in-range, and this is the cheapest possible guarantee that it is.
        ...clampResources(resources, selected),
      },
      { onSuccess: (m) => navigate(machineOverviewPath(m.id)) },
    );
  };

  // The page keeps its own title while the catalog loads. A page that renders
  // as a bare spinner has no identity in the browser's history or the reader's
  // eye, and this one is reached by an explicit "Launch machine" click.
  if (templates.isLoading) {
    return (
      <div className="c-page">
        <header className="c-page-header">
          <h1 className="c-page-title">Launch machine</h1>
        </header>
        <p className="c-muted">Loading templates…</p>
      </div>
    );
  }

  if (templates.isError || catalog.length === 0) {
    return (
      <div className="c-page">
        <header className="c-page-header">
          <h1 className="c-page-title">Launch machine</h1>
        </header>
        <div className="c-panel">
          <p className="c-muted">
            {templates.isError
              ? 'Could not load the template catalog.'
              : 'No machine templates are available. This is a deployment configuration issue — the catalog is set on the control plane.'}
          </p>
          <Link className="c-btn" to={MACHINES}>
            Back to machines
          </Link>
        </div>
      </div>
    );
  }

  return (
    <div className="c-page">
      <header className="c-page-header">
        <h1 className="c-page-title">Launch machine</h1>
        <ol className="c-steps">
          {WIZARD_STEPS.map((s, i) => (
            <li
              key={s.key}
              className={
                i === stepIndex
                  ? 'c-step c-step-current'
                  : i < stepIndex
                    ? 'c-step c-step-done'
                    : 'c-step'
              }
              aria-current={i === stepIndex ? 'step' : undefined}
            >
              <span className="c-step-index">{i + 1}</span>
              {s.title}
            </li>
          ))}
        </ol>
      </header>

      {atLimit && (
        <p className="c-banner c-banner-error" role="alert">
          You are using all {me.machine_limit} of your machines. Terminate one before launching
          another.
        </p>
      )}

      <div className="c-panel c-wizard-panel">
        {step.key === 'name' && (
          <>
            <label className="c-field">
              <span className="c-field-label">Name</span>
              <input
                type="text"
                className="c-input"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Leave blank to generate one"
                aria-label="Machine name"
              />
            </label>
            <p className="c-muted c-field-hint">
              Optional. A blank name is generated by the control plane, and can be changed later.
            </p>
          </>
        )}

        {step.key === 'template' && (
          <ul className="c-template-list">
            {catalog.map((t) => (
              <li key={t.id}>
                <button
                  type="button"
                  className={t.id === templateId ? 'c-template c-template-selected' : 'c-template'}
                  onClick={() => chooseTemplate(t)}
                  aria-pressed={t.id === templateId}
                >
                  <span className="c-template-label">{t.label}</span>
                  <span className="c-muted c-template-desc">{t.description}</span>
                  <span className="c-muted c-template-spec">
                    {t.defaults.vcpus} vCPU · {gib(t.defaults.mem_mib)} · {gib(t.defaults.disk_mib)}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        )}

        {step.key === 'size' && selected && resources && (
          <>
            <ResourceSlider
              label="vCPUs"
              value={resources.vcpus}
              min={selected.limits.vcpus.min}
              max={selected.limits.vcpus.max}
              step={1}
              format={(v) => `${v}`}
              onChange={(v) => setResource('vcpus', v)}
            />
            <ResourceSlider
              label="Memory"
              value={resources.mem_mib}
              min={selected.limits.mem_mib.min}
              max={selected.limits.mem_mib.max}
              step={512}
              format={gib}
              onChange={(v) => setResource('mem_mib', v)}
            />
            <ResourceSlider
              label="Disk"
              value={resources.disk_mib}
              min={selected.limits.disk_mib.min}
              max={selected.limits.disk_mib.max}
              step={1024}
              format={gib}
              onChange={(v) => setResource('disk_mib', v)}
            />
            <p className="c-muted c-field-hint">
              Ranges come from the {selected.label} template. Disk is persistent and survives a
              stop.
            </p>
          </>
        )}

        {step.key === 'review' && selected && resources && (
          <dl className="c-defs">
            <dt>Name</dt>
            <dd>{name.trim() || <span className="c-muted">Generated on launch</span>}</dd>
            <dt>Template</dt>
            <dd>{selected.label}</dd>
            <dt>vCPUs</dt>
            <dd>{resources.vcpus}</dd>
            <dt>Memory</dt>
            <dd>{gib(resources.mem_mib)}</dd>
            <dt>Disk</dt>
            <dd>{gib(resources.disk_mib)}</dd>
          </dl>
        )}

        {create.isError && (
          <p className="c-banner c-banner-error" role="alert">
            {describeApiError(create.error, 'Could not launch the machine.')}
          </p>
        )}

        <div className="c-wizard-actions">
          <Link className="c-btn" to={MACHINES}>
            Cancel
          </Link>
          <div className="c-toolbar-spacer" />
          <button
            type="button"
            className="c-btn"
            disabled={stepIndex === 0}
            onClick={() => setStepIndex((i) => i - 1)}
          >
            Back
          </button>
          {step.key === 'review' ? (
            <button
              type="button"
              className="c-btn c-btn-primary"
              disabled={create.isPending || atLimit}
              onClick={submit}
            >
              {create.isPending ? 'Launching…' : 'Launch'}
            </button>
          ) : (
            <button
              type="button"
              className="c-btn c-btn-primary"
              disabled={!canAdvance}
              onClick={() => setStepIndex((i) => i + 1)}
            >
              Next
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

function ResourceSlider({
  label,
  value,
  min,
  max,
  step,
  format,
  onChange,
}: {
  label: string;
  value: number;
  min: number;
  max: number;
  step: number;
  format: (v: number) => string;
  onChange: (v: number) => void;
}) {
  return (
    <div className="c-field">
      <div className="c-slider-head">
        <span className="c-field-label">{label}</span>
        <span className="c-slider-value">{format(value)}</span>
      </div>
      <input
        type="range"
        className="c-range"
        min={min}
        max={max}
        step={step}
        value={value}
        onChange={(e) => onChange(Number(e.target.value))}
        aria-label={label}
      />
      <div className="c-slider-bounds c-muted">
        <span>{format(min)}</span>
        <span>{format(max)}</span>
      </div>
    </div>
  );
}
