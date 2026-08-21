import { useState } from 'react';
import { ApiError, type MachineSummary } from '../../api/client';
import { useMachineMutations } from '../../api/hooks';
import { failures, runBulk } from '../bulk';
import { describeApiError } from '../errors';
import { Dialog } from '../ui/Dialog';

interface Failure {
  machine: MachineSummary;
  message: string;
  // exportBlocked marks the one failure the user can override: the control
  // plane refused because it could not export the machine's coding-agent
  // sessions first. Everything else is a plain error and force would not help.
  exportBlocked: boolean;
}

type Step = 'confirm' | 'running' | 'blocked';

// TerminateDialog is the guarded destroy flow, scaled to a selection.
//
// Destroy is not a simple delete: the control plane exports the machine's Claude
// coding-agent sessions before tearing it down, and refuses with
// session_export_failed if that fails. That refusal is a feature — it is the
// last chance to not lose the transcript — so it surfaces as a distinct
// "Force delete anyway" rather than a generic retry.
//
// Partial failure is the normal case for a selection, so the dialog reports
// per-machine rather than collapsing to "something went wrong": the machines
// that succeeded are gone, and the user needs to know which ones those were.
export function TerminateDialog({
  machines,
  onClose,
}: {
  machines: MachineSummary[];
  onClose: () => void;
}) {
  const { destroy } = useMachineMutations();
  const [step, setStep] = useState<Step>('confirm');
  const [done, setDone] = useState(0);
  const [failed, setFailed] = useState<Failure[]>([]);

  const run = async (targets: MachineSummary[], force: boolean) => {
    setStep('running');
    setDone(0);
    setFailed([]);

    const outcomes = await runBulk(targets, async (m) => {
      await destroy.mutateAsync({ id: m.id, force });
      setDone((d) => d + 1);
    });

    const bad: Failure[] = failures(outcomes).map((o) => ({
      machine: o.item,
      message: describeApiError(o.error, 'Could not destroy this machine.'),
      exportBlocked: o.error instanceof ApiError && o.error.code === 'session_export_failed',
    }));

    if (bad.length === 0) {
      onClose();
      return;
    }
    setFailed(bad);
    setStep('blocked');
  };

  const plural = machines.length === 1 ? 'machine' : 'machines';
  // Closing mid-run would hide a destroy that keeps going; block it.
  const dismissable = step !== 'running';
  const anyExportBlocked = failed.some((f) => f.exportBlocked);

  return (
    <Dialog
      title={`Terminate ${machines.length} ${plural}`}
      onClose={dismissable ? onClose : () => {}}
      dismissable={dismissable}
    >
      {step === 'confirm' && (
        <>
          <p className="c-dialog-warning">
            This destroys{' '}
            {machines.length === 1 ? 'this machine' : `these ${machines.length} machines`} and wipes{' '}
            {machines.length === 1 ? 'its' : 'their'} persistent disk. Coding-agent sessions are
            exported first. This cannot be undone.
          </p>
          <ul className="c-dialog-list">
            {machines.map((m) => (
              <li key={m.id}>{m.name}</li>
            ))}
          </ul>
          <div className="c-dialog-actions">
            <button type="button" className="c-btn" onClick={onClose}>
              Cancel
            </button>
            <button
              type="button"
              className="c-btn c-btn-danger"
              onClick={() => void run(machines, false)}
            >
              Terminate
            </button>
          </div>
        </>
      )}

      {step === 'running' && (
        <p className="c-muted">
          Exporting sessions and terminating… {done} of {machines.length} done.
        </p>
      )}

      {step === 'blocked' && (
        <>
          <p className="c-dialog-warning">
            {machines.length - failed.length} of {machines.length} terminated.{' '}
            {failed.length === 1 ? 'One failed' : `${failed.length} failed`}:
          </p>
          <ul className="c-dialog-list">
            {failed.map((f) => (
              <li key={f.machine.id}>
                <strong>{f.machine.name}</strong> — {f.message}
              </li>
            ))}
          </ul>
          <div className="c-dialog-actions">
            <button type="button" className="c-btn" onClick={onClose}>
              Close
            </button>
            <button
              type="button"
              className="c-btn c-btn-danger"
              onClick={() =>
                void run(
                  failed.map((f) => f.machine),
                  anyExportBlocked,
                )
              }
            >
              {anyExportBlocked ? 'Force delete anyway' : 'Retry'}
            </button>
          </div>
        </>
      )}
    </Dialog>
  );
}
