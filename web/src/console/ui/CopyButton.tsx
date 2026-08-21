import { useState } from 'react';

// CopyButton copies a value and says so for a moment. Used for machine ids,
// which exist to be pasted into the CLI or a support message.
//
// navigator.clipboard is absent on insecure origins and in some embedders, so
// the button hides itself rather than offering an action that silently does
// nothing — the id is selectable text either way.
export function CopyButton({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false);
  if (typeof navigator === 'undefined' || !navigator.clipboard) return null;

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // Denied by permissions policy; leave the button silent rather than
      // raising an error for something the user can do by hand.
    }
  };

  return (
    <button type="button" className="c-copy" onClick={() => void copy()} aria-label={label}>
      {copied ? 'Copied' : 'Copy'}
    </button>
  );
}
