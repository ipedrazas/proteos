import { useEffect, type ReactNode } from 'react';
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
  useEffect(() => {
    if (!dismissable) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
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
