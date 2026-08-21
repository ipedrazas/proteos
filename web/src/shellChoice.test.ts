import { describe, expect, it } from 'vitest';
import { resolveShell, setShell } from './shellChoice';

function memStorage(initial: Record<string, string> = {}) {
  const map = new Map(Object.entries(initial));
  return {
    getItem: (k: string) => map.get(k) ?? null,
    setItem: (k: string, v: string) => void map.set(k, v),
    removeItem: (k: string) => void map.delete(k),
    dump: () => Object.fromEntries(map),
  };
}

describe('resolveShell', () => {
  it('defaults to the console with no preference and no query param', () => {
    expect(resolveShell('', memStorage())).toBe('console');
  });

  it('honours a stored desktop preference', () => {
    expect(resolveShell('', memStorage({ 'proteos-ui-shell': 'desktop' }))).toBe('desktop');
  });

  it('honours a stored console preference', () => {
    expect(resolveShell('', memStorage({ 'proteos-ui-shell': 'console' }))).toBe('console');
  });

  it('?shell=desktop selects the desktop and remembers it', () => {
    const storage = memStorage();
    expect(resolveShell('?shell=desktop', storage)).toBe('desktop');
    expect(resolveShell('', storage)).toBe('desktop');
  });

  it('?shell=console selects the console and remembers it', () => {
    const storage = memStorage({ 'proteos-ui-shell': 'desktop' });
    expect(resolveShell('?shell=console', storage)).toBe('console');
    expect(resolveShell('', storage)).toBe('console');
  });

  it('ignores an unrecognised ?shell= value instead of clearing the preference', () => {
    const storage = memStorage({ 'proteos-ui-shell': 'desktop' });
    expect(resolveShell('?shell=banana', storage)).toBe('desktop');
    expect(storage.dump()).toEqual({ 'proteos-ui-shell': 'desktop' });
  });

  it('treats an unknown stored value as the console default', () => {
    expect(resolveShell('', memStorage({ 'proteos-ui-shell': 'banana' }))).toBe('console');
  });
});

describe('setShell', () => {
  it('stores both choices verbatim, including the default', () => {
    // Storing only the non-default made "chose the default" and "never chose"
    // indistinguishable, so changing the default silently moved people who had
    // picked the old one.
    const storage = memStorage();
    setShell(storage, 'desktop');
    expect(storage.dump()).toEqual({ 'proteos-ui-shell': 'desktop' });
    setShell(storage, 'console');
    expect(storage.dump()).toEqual({ 'proteos-ui-shell': 'console' });
  });
});
