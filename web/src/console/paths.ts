// paths is the console's URL shape, in one place.
//
// Every console path is written absolute. The console mounts under a splat
// route (/console/*), so a relative target resolves against the matched
// pathname, not the mount point: a relative "machines" from /console/keys
// resolves to /console/keys/machines. Absolute paths are unambiguous, and the
// console has exactly one mount point to be absolute about.
//
// Separate from nav.tsx because these are functions, not constants, and a file
// exporting both a component and a function loses React Fast Refresh.

export const MACHINES = '/console/machines';
export const MACHINES_LAUNCH = '/console/machines/launch';

// The id is encoded: it is a UUID today, but a path helper that assumes its
// input is URL-safe is a bug waiting for the day it is not.
export function machinePath(id: string): string {
  return `${MACHINES}/${encodeURIComponent(id)}`;
}

// The machine detail tabs, in display order. One array drives the tab strip,
// the route's validation, and the path helper, so a tab cannot exist in the URL
// without existing in the strip.
export const MACHINE_TABS = [
  { key: 'overview', label: 'Overview' },
  { key: 'connect', label: 'Connect' },
  { key: 'terminal', label: 'Terminal' },
] as const;

export type MachineTab = (typeof MACHINE_TABS)[number]['key'];

export function isMachineTab(value: string | undefined): value is MachineTab {
  return MACHINE_TABS.some((t) => t.key === value);
}

export function machineTabPath(id: string, tab: MachineTab): string {
  return `${machinePath(id)}/${tab}`;
}

export function machineOverviewPath(id: string): string {
  return machineTabPath(id, 'overview');
}
