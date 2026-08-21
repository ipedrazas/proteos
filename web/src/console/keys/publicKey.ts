// publicKey validates an OpenSSH public key before it is sent to the control
// plane.
//
// The server validates too, and its answer is authoritative. This exists for
// one case the server cannot help with: a user pasting their PRIVATE key. By
// the time the server rejects it, the secret has already left the machine, been
// through a TLS terminator and a reverse proxy, and quite possibly landed in an
// access log. Catching it in the browser is the only place it is still private.
//
// Everything else here is ordinary "tell them what is wrong before the round
// trip" politeness.

// The key types OpenSSH emits today. DSA is deliberately absent — it is
// disabled by default in modern OpenSSH, so accepting it here would produce a
// key that is registered and then silently never works.
export const SUPPORTED_KEY_TYPES = [
  'ssh-ed25519',
  'ssh-rsa',
  'ecdsa-sha2-nistp256',
  'ecdsa-sha2-nistp384',
  'ecdsa-sha2-nistp521',
  'sk-ssh-ed25519@openssh.com',
  'sk-ecdsa-sha2-nistp256@openssh.com',
] as const;

export type KeyCheck =
  { ok: true; key: string; type: string; comment: string } | { ok: false; error: string };

// A base64 blob, and long enough that a truncated paste is caught. The shortest
// real key (ed25519) is ~68 base64 characters; 40 is a floor no valid key trips.
const BLOB = /^[A-Za-z0-9+/]+={0,3}$/;
const MIN_BLOB = 40;

export function checkPublicKey(input: string): KeyCheck {
  const text = input.trim();
  if (text === '') return { ok: false, error: 'Paste a public key.' };

  // Checked before anything else, and phrased as an instruction rather than a
  // validation message: the user needs to stop and think, not correct a field.
  if (/-----BEGIN[\w ]*PRIVATE KEY-----/.test(text) || text.startsWith('-----BEGIN')) {
    return {
      ok: false,
      error:
        'That is a private key — never paste or share it. You want the matching .pub file, e.g. ~/.ssh/id_ed25519.pub.',
    };
  }

  if (/[\r\n]/.test(text)) {
    return {
      ok: false,
      error:
        'That key is split across lines. A public key is a single line — paste the whole file.',
    };
  }

  const parts = text.split(/\s+/);
  const [type, blob, ...rest] = parts;

  if (!(SUPPORTED_KEY_TYPES as readonly string[]).includes(type)) {
    return {
      ok: false,
      error: `Unrecognised key type "${type.slice(0, 30)}". Supported: ${SUPPORTED_KEY_TYPES.join(', ')}.`,
    };
  }

  if (!blob || !BLOB.test(blob) || blob.length < MIN_BLOB) {
    return { ok: false, error: 'That key looks incomplete — copy the whole .pub file.' };
  }

  const comment = rest.join(' ');
  // Normalised: exactly one space between fields, no trailing whitespace, so
  // two pastes of the same key cannot land as two different rows.
  return {
    ok: true,
    key: comment ? `${type} ${blob} ${comment}` : `${type} ${blob}`,
    type,
    comment,
  };
}

// suggestLabel turns a key's trailing comment — usually "user@host" from
// ssh-keygen — into a default label, so the common case needs no typing.
export function suggestLabel(comment: string): string {
  return comment.trim().slice(0, 60);
}
