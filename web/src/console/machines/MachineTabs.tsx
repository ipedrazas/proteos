import { NavLink } from 'react-router';
import { MACHINE_TABS, machineTabPath } from '../paths';

// MachineTabs is the detail page's tab strip. Links rather than local state, so
// a tab is a URL: shareable, bookmarkable, and survivable across a refresh —
// which matters most for the terminal, the one tab someone leaves open.
export function MachineTabs({ id, active }: { id: string; active: string }) {
  return (
    <nav className="c-tabs" role="tablist" aria-label="Machine sections">
      {MACHINE_TABS.map((tab) => (
        <NavLink
          key={tab.key}
          to={machineTabPath(id, tab.key)}
          role="tab"
          aria-selected={tab.key === active}
          className={tab.key === active ? 'c-tab c-tab-active' : 'c-tab'}
        >
          {tab.label}
        </NavLink>
      ))}
    </nav>
  );
}
