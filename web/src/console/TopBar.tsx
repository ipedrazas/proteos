import { useRef, useState } from 'react';
import { useNavigate } from 'react-router';
import type { Me } from '../api/client';
import { useLogout } from '../api/hooks';
import { useClickOutside } from '../lib/useClickOutside';
import { switchShell } from '../shellChoice';

// TopBar is the console's global chrome: the brand mark on the left and the
// account menu on the right. The account menu is also where a user leaves the
// console for the desktop shell — the switch is a first-class, always-reachable
// action while the two UIs run side by side, not a hidden URL trick.
export function TopBar({ me }: { me: Me }) {
  const navigate = useNavigate();
  const logout = useLogout();
  const [open, setOpen] = useState(false);
  const buttonRef = useRef<HTMLButtonElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);
  useClickOutside(open, [buttonRef, menuRef], () => setOpen(false));

  const toDesktop = () => {
    switchShell('desktop');
    navigate('/');
  };

  const signOut = () => {
    logout.mutate(undefined, {
      onSettled: () => navigate('/login', { replace: true }),
    });
  };

  return (
    <header className="c-topbar">
      <div className="c-brand">
        {/* The Proteus wave, matching the desktop rail's brand mark. */}
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" aria-hidden>
          <path
            d="M3 14c3-3 6-3 9 0s6 3 9 0M3 8c3-3 6-3 9 0s6 3 9 0"
            stroke="currentColor"
            strokeWidth="1.9"
            strokeLinecap="round"
          />
        </svg>
        <span className="c-brand-name">ProteOS</span>
        <span className="c-brand-sub">Console</span>
      </div>

      <div className="c-topbar-spacer" />

      <button
        type="button"
        className="c-account-button"
        ref={buttonRef}
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
      >
        {me.user.avatar_url && <img src={me.user.avatar_url} alt="" width={24} height={24} />}
        <span className="c-account-login">{me.user.login}</span>
      </button>

      {open && (
        <div className="c-account-menu" role="menu" ref={menuRef}>
          <div className="c-account-email">{me.user.email}</div>
          <div className="c-menu-sep" />
          <button type="button" className="c-menu-item" role="menuitem" onClick={toDesktop}>
            Switch to desktop UI
          </button>
          <div className="c-menu-sep" />
          <button
            type="button"
            className="c-menu-item"
            role="menuitem"
            onClick={signOut}
            disabled={logout.isPending}
          >
            {logout.isPending ? 'Signing out…' : 'Sign out'}
          </button>
        </div>
      )}
    </header>
  );
}
