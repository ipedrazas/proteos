import type { MachineState } from '../../api/client';
import { describeState } from '../machineState';

// StateBadge is the machine-state pill: a coloured dot plus a label, with the
// dot animated while the control plane is mid-transition.
//
// The precise API state is the title, not the label. The label collapses eight
// states into four so a fleet can be scanned at a glance (see machineState);
// the title is there for the moment someone needs to know whether "Pending"
// means requested, provisioning, or starting.
export function StateBadge({ state }: { state: MachineState }) {
  const { label, tone, busy } = describeState(state);
  return (
    <span className={`c-badge c-badge-${tone}`} title={state}>
      <span className={busy ? 'c-badge-dot c-badge-dot-busy' : 'c-badge-dot'} aria-hidden />
      {label}
    </span>
  );
}
