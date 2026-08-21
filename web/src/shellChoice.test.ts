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
  it('defaults to the desktop with no preference and no query param', () => {
    expect(resolveShell('', memStorage())).toBe('desktop');
  });

  it('honours a stored console preference', () => {
    expect(resolveShell('', memStorage({ 'proteos-ui-shell': 'console' }))).toBe('console');
  });

  it('?shell=console selects the console and remembers it', () => {
    const storage = memStorage();
    expect(resolveShell('?shell=console', storage)).toBe('console');
    expect(resolveShell('', storage)).toBe('console');
  });

  it('?shell=desktop selects the desktop and clears the stored preference', () => {
    const storage = memStorage({ 'proteos-ui-shell': 'console' });
    expect(resolveShell('?shell=desktop', storage)).toBe('desktop');
    expect(storage.dump()).toEqual({});
    expect(resolveShell('', storage)).toBe('desktop');
  });

  it('ignores an unrecognised ?shell= value instead of clearing the preference', () => {
    const storage = memStorage({ 'proteos-ui-shell': 'console' });
    expect(resolveShell('?shell=banana', storage)).toBe('console');
    expect(storage.dump()).toEqual({ 'proteos-ui-shell': 'console' });
  });

  it('treats an unknown stored value as the desktop default', () => {
    expect(resolveShell('', memStorage({ 'proteos-ui-shell': 'banana' }))).toBe('desktop');
  });
});

describe('setShell', () => {
  it('stores the console choice and clears it for the desktop', () => {
    const storage = memStorage();
    setShell(storage, 'console');
    expect(storage.dump()).toEqual({ 'proteos-ui-shell': 'console' });
    setShell(storage, 'desktop');
    expect(storage.dump()).toEqual({});
  });
});
