import { useEffect, useRef, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

// Dialog is the console's modal. It portals to document.body so it is never
// clipped or positioned by whatever scroll container invoked it.
//
// The portal escapes .c-app, and every console style is scoped under .c-app —
// so the wrapper carries `c-portal`, which the stylesheet pairs with .c-app
// wherever it defines tokens. Without that the dialog renders unstyled, which
// is the failure mode scoping always has and the reason it is called out here.
//
// dismissable is false while a destructive action is mid-flight: Escape and a
// backdrop click would otherwise hide a running terminate, and the machines
// would keep disappearing from under a user who thinks they cancelled.

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

export function Dialog({
  title,
  onClose,
  dismissable = true,
  children,
}: {
  title: string;
  onClose: () => void;
  dismissable?: boolean;
  children: ReactNode;
}) {
  const panelRef = useRef<HTMLDivElement | null>(null);

  // Move focus into the dialog on open and put it back where it came from on
  // close. Without this the keyboard stays behind the modal: Tab walks the page
  // underneath, and dismissing leaves focus on <body>, so the next Tab starts
  // over from the top of the document.
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    const panel = panelRef.current;
    const first = panel?.querySelector<HTMLElement>(FOCUSABLE);
    (first ?? panel)?.focus();
    return () => previous?.focus?.();
  }, []);

  // Escape to dismiss, and Tab cycling kept inside the panel. A modal that lets
  // Tab escape is worse than one with no keyboard support at all — the focus
  // ring disappears behind the backdrop with no way to tell where it went.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && dismissable) {
        onClose();
        return;
      }
      if (e.key !== 'Tab') return;
      const panel = panelRef.current;
      if (!panel) return;
      const items = [...panel.querySelectorAll<HTMLElement>(FOCUSABLE)];
      if (items.length === 0) return;
      const first = items[0];
      const last = items[items.length - 1];
      const active = document.activeElement;
      if (e.shiftKey && (active === first || active === panel)) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [dismissable, onClose]);

  return createPortal(
    <div className="c-portal">
      <div className="c-dialog-backdrop" onClick={dismissable ? onClose : undefined}>
        <div
          className="c-dialog"
          role="dialog"
          aria-modal="true"
          aria-label={title}
          tabIndex={-1}
          ref={panelRef}
          onClick={(e) => e.stopPropagation()}
        >
          <h2 className="c-dialog-title">{title}</h2>
          {children}
        </div>
      </div>
    </div>,
    document.body,
  );
}
