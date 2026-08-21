// shellChoice remembers which top-level shell a visit to "/" should land on:
// the desktop window manager (the default) or the console. It is the sibling of
// mobileGate.ts — that one picks desktop vs phone from the viewport, this one
// picks desktop vs console from an explicit user choice.
//
// The two are deliberately independent and mobileGate runs FIRST, so a phone
// still gets the mobile shell regardless of what was chosen here on a laptop.
//
// Escape hatch: "/?shell=console" and "/?shell=desktop" set the preference and
// are what the in-app switchers navigate to.

export type Shell = 'desktop' | 'console';

const SHELL_KEY = 'proteos-ui-shell';

type ShellStorage = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;

// setShell persists the user's choice. Desktop is stored as ABSENCE of the key
// rather than the string "desktop": it is the default, so forgetting the
// preference and choosing the default are the same state — one less way for the
// two to disagree after a default change.
export function setShell(storage: ShellStorage, shell: Shell): void {
  if (shell === 'console') storage.setItem(SHELL_KEY, 'console');
  else storage.removeItem(SHELL_KEY);
}

// resolveShell is the pure decision for a visit to "/": an explicit ?shell=
// query param wins and is remembered; otherwise the stored preference applies;
// otherwise the desktop. An unrecognised ?shell= value is ignored rather than
// treated as a choice, so a typo does not silently clear the preference.
export function resolveShell(search: string, storage: ShellStorage): Shell {
  const requested = new URLSearchParams(search).get('shell');
  if (requested === 'console' || requested === 'desktop') {
    setShell(storage, requested);
    return requested;
  }
  return storage.getItem(SHELL_KEY) === 'console' ? 'console' : 'desktop';
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
