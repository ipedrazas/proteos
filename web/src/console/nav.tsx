import { NavLink } from 'react-router';

// SideNav is the console's primary navigation: a persistent, grouped list of
// services, AWS-style. Unlike the desktop's LeftRail it navigates to routes
// rather than opening windows, and nothing here depends on a "current machine"
// — every entry is a resource collection that stands on its own.

// Every console path is written absolute. Relative targets resolve against the
// matched pathname, and the console is mounted under a splat route (/console/*)
// — so a relative "machines" from /console/keys resolves to
// /console/keys/machines rather than /console/machines. Absolute paths are
// unambiguous, and the console has exactly one mount point to be absolute about.
export const MACHINES = '/console/machines';

interface NavItem {
  to: string;
  label: string;
  // adminOnly entries render only for a user holding the proteos.admin role.
  // This is presentation only: every /api/admin route re-checks the role
  // server-side, so a client that forces this true gets an empty page and a 403.
  adminOnly?: boolean;
}

interface NavGroup {
  title: string;
  items: NavItem[];
}

const NAV_GROUPS: NavGroup[] = [
  {
    title: 'Compute',
    items: [
      { to: MACHINES, label: 'Machines' },
      { to: '/console/templates', label: 'Templates' },
    ],
  },
  {
    title: 'Access',
    items: [
      { to: '/console/keys', label: 'SSH keys' },
      { to: '/console/secrets', label: 'Secrets' },
    ],
  },
  {
    title: 'Operations',
    items: [
      { to: '/console/activity', label: 'Activity' },
      { to: '/console/admin', label: 'Admin', adminOnly: true },
    ],
  },
];

export function SideNav({ isAdmin }: { isAdmin: boolean }) {
  return (
    <nav className="c-sidenav" aria-label="Console">
      {NAV_GROUPS.map((group) => {
        const items = group.items.filter((i) => !i.adminOnly || isAdmin);
        if (items.length === 0) return null;
        return (
          <div className="c-nav-group" key={group.title}>
            <h2 className="c-nav-group-title">{group.title}</h2>
            <ul className="c-nav-list">
              {items.map((item) => (
                <li key={item.to}>
                  <NavLink
                    to={item.to}
                    className={({ isActive }) =>
                      isActive ? 'c-nav-item c-nav-item-active' : 'c-nav-item'
                    }
                  >
                    {item.label}
                  </NavLink>
                </li>
              ))}
            </ul>
          </div>
        );
      })}
    </nav>
  );
}
