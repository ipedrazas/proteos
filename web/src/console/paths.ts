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

export function machineOverviewPath(id: string): string {
  return `${machinePath(id)}/overview`;
}
