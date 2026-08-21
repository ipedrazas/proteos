// wizard holds the launch flow's pure logic: the resource maths and the step
// model. Both are separated from the component because they are where the
// wizard can be silently wrong — a clamp that lets a value past its cap sends
// the control plane a request it will reject with invalid_resources, and the
// user sees a server error for something the form should never have allowed.

import type { MachineTemplate } from '../../api/client';

export interface Resources {
  vcpus: number;
  mem_mib: number;
  disk_mib: number;
}

// The four steps, in order. The array is the source of truth for both the
// progress indicator and the next/back arithmetic, so they cannot disagree.
export const WIZARD_STEPS = [
  { key: 'name', title: 'Name' },
  { key: 'template', title: 'Template' },
  { key: 'size', title: 'Size' },
  { key: 'review', title: 'Review' },
] as const;

// clamp pins a value into an inclusive [min,max] range, rounding to an integer.
//
// NaN — what an emptied number field yields — collapses to min. It has to be
// handled explicitly because NaN silently fails every comparison, so it would
// otherwise fall straight through Math.min/Math.max and reach the API. "The
// smallest allowed machine" is a safe reading of "nothing entered".
//
// ±Infinity deliberately does NOT take that path: it is a value that IS outside
// the range in a known direction, so it clamps to the bound it exceeds like any
// other out-of-range number.
export function clamp(value: number, min: number, max: number): number {
  if (Number.isNaN(value)) return min;
  return Math.min(max, Math.max(min, Math.round(value)));
}

// clampResources pins a whole spec into the template's caps. Applied on every
// change and again before submit, so a template switch cannot leave a value
// from the previous template's wider range in place.
export function clampResources(resources: Resources, template: MachineTemplate): Resources {
  const { limits } = template;
  return {
    vcpus: clamp(resources.vcpus, limits.vcpus.min, limits.vcpus.max),
    mem_mib: clamp(resources.mem_mib, limits.mem_mib.min, limits.mem_mib.max),
    disk_mib: clamp(resources.disk_mib, limits.disk_mib.min, limits.disk_mib.max),
  };
}

// defaultResources is a template's own defaults, clamped by its own limits. The
// clamp is not paranoia about our own code — the catalog is operator-authored
// config (docs/architecture.md), so a template whose defaults sit outside its
// limits is a deployment typo the UI should absorb rather than propagate.
export function defaultResources(template: MachineTemplate): Resources {
  return clampResources(template.defaults, template);
}
