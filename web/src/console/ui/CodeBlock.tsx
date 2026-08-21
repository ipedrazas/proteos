import { CopyButton } from './CopyButton';

// CodeBlock is a command the reader is meant to run: monospace, selectable, and
// copyable in one click. Multi-line blocks (an ~/.ssh/config stanza) keep their
// newlines both on screen and on the clipboard.
export function CodeBlock({ text, label }: { text: string; label: string }) {
  return (
    <div className="c-code">
      <pre className="c-code-text">{text}</pre>
      <CopyButton value={text} label={label} />
    </div>
  );
}
