import { describe, expect, it } from 'vitest';
import { checkPublicKey, suggestLabel } from './publicKey';

const ED25519 = 'AAAAC3NzaC1lZDI1NTE5AAAAIJb1Kx8QGvRk9pT2mZq4wX7yNcLdEfGhIjKlMnOpQrSt';

describe('checkPublicKey', () => {
  it('accepts an ed25519 key with a comment', () => {
    const r = checkPublicKey(`ssh-ed25519 ${ED25519} ada@laptop`);
    expect(r.ok).toBe(true);
    if (r.ok) {
      expect(r.type).toBe('ssh-ed25519');
      expect(r.comment).toBe('ada@laptop');
    }
  });

  it('accepts a key with no comment', () => {
    const r = checkPublicKey(`ssh-ed25519 ${ED25519}`);
    expect(r.ok).toBe(true);
    if (r.ok) expect(r.comment).toBe('');
  });

  it('normalises spacing so one key cannot land as two rows', () => {
    const r = checkPublicKey(`  ssh-ed25519    ${ED25519}   ada@laptop  `);
    expect(r.ok).toBe(true);
    if (r.ok) expect(r.key).toBe(`ssh-ed25519 ${ED25519} ada@laptop`);
  });

  it('keeps a comment containing spaces', () => {
    const r = checkPublicKey(`ssh-ed25519 ${ED25519} work laptop 2026`);
    expect(r.ok).toBe(true);
    if (r.ok) expect(r.comment).toBe('work laptop 2026');
  });

  it('accepts every supported type', () => {
    for (const type of ['ssh-rsa', 'ecdsa-sha2-nistp256', 'sk-ssh-ed25519@openssh.com']) {
      expect(checkPublicKey(`${type} ${ED25519}`).ok).toBe(true);
    }
  });

  it('rejects an empty paste', () => {
    expect(checkPublicKey('   ')).toEqual({ ok: false, error: 'Paste a public key.' });
  });

  it('refuses a private key, and says why it matters', () => {
    // The whole reason this module exists: the server would reject it too, but
    // only after the secret has crossed the network.
    const priv =
      '-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1r\n-----END OPENSSH PRIVATE KEY-----';
    const r = checkPublicKey(priv);
    expect(r.ok).toBe(false);
    if (!r.ok) {
      expect(r.error).toContain('private key');
      expect(r.error).toContain('never');
    }
  });

  it('refuses an RSA private key header too', () => {
    const r = checkPublicKey(
      '-----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY-----',
    );
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error).toContain('private key');
  });

  it('rejects a key split across lines rather than silently mangling it', () => {
    const r = checkPublicKey(
      `ssh-ed25519 ${ED25519.slice(0, 30)}\n${ED25519.slice(30)} ada@laptop`,
    );
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error).toContain('single line');
  });

  it('rejects an unsupported type and lists what is supported', () => {
    const r = checkPublicKey(`ssh-dss ${ED25519}`);
    expect(r.ok).toBe(false);
    // DSA is disabled by default in modern OpenSSH — accepting it would
    // register a key that then silently never works.
    if (!r.ok) expect(r.error).toContain('ssh-ed25519');
  });

  it('rejects a truncated blob', () => {
    const r = checkPublicKey('ssh-ed25519 AAAAC3Nza');
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error).toContain('incomplete');
  });

  it('rejects a type with no blob at all', () => {
    expect(checkPublicKey('ssh-ed25519').ok).toBe(false);
  });

  it('rejects a blob with characters base64 cannot contain', () => {
    expect(checkPublicKey(`ssh-ed25519 ${'!'.repeat(60)}`).ok).toBe(false);
  });
});

describe('suggestLabel', () => {
  it('uses the key comment, which is usually user@host', () => {
    expect(suggestLabel('ada@laptop')).toBe('ada@laptop');
  });

  it('is empty for a key with no comment', () => {
    expect(suggestLabel('')).toBe('');
  });

  it('caps a very long comment', () => {
    expect(suggestLabel('x'.repeat(200)).length).toBe(60);
  });
});
