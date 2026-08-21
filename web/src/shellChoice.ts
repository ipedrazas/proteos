// shellChoice remembers which top-level shell a visit to "/" should land on:
// the console (the default) or the desktop window manager. It is the sibling of
// mobileGate.ts — that one picks desktop vs phone from the viewport, this one
// picks console vs desktop from an explicit user choice.
//
// The two are deliberately independent and mobileGate runs FIRST, so a phone
// still gets the mobile shell regardless of what was chosen here on a laptop.
//
// Escape hatch: "/?shell=console" and "/?shell=desktop" set the preference and
// are what the in-app switchers navigate to.

export type Shell = 'console' | 'desktop';

const SHELL_KEY = 'proteos-ui-shell';

type ShellStorage = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;

// setShell stores the choice VERBATIM, including the default.
//
// The first version stored only the non-default and read absence as "whatever
// the default is". That works right up until the default changes — at which
// point everyone who had explicitly chosen the old default is indistinguishable
// from everyone who never chose at all, and gets silently moved. Storing the
// actual choice means the next default change, if there ever is one, cannot do
// that to anyone who has expressed a preference since this landed.
export function setShell(storage: ShellStorage, shell: Shell): void {
  storage.setItem(SHELL_KEY, shell);
}

// resolveShell is the pure decision for a visit to "/": an explicit ?shell=
// query param wins and is remembered; otherwise the stored preference applies;
// otherwise the console. An unrecognised stored or requested value falls through
// to the default rather than being treated as a choice.
export function resolveShell(search: string, storage: ShellStorage): Shell {
  const requested = new URLSearchParams(search).get('shell');
  if (requested === 'console' || requested === 'desktop') {
    setShell(storage, requested);
    return requested;
  }
  return storage.getItem(SHELL_KEY) === 'desktop' ? 'desktop' : 'console';
}

// switchShell is what the in-app switchers call before navigating: remember the
// choice, tolerating a storage that throws (privacy mode, odd embedders) so the
// navigation that follows still happens.
export function switchShell(shell: Shell): void {
  try {
    setShell(window.localStorage, shell);
  } catch {
    // Preference not remembered; the explicit navigation still takes effect.
  }
}
