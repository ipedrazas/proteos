import { NavLink } from 'react-router';
import { MACHINE_TABS, machineTabPath } from '../paths';

// MachineTabs is the detail page's section switcher. Links rather than local
// state, so a section is a URL: shareable, bookmarkable, and survivable across a
// refresh — which matters most for the terminal, the one section someone leaves
// open.
//
// Deliberately NOT role="tablist"/role="tab". That contract promises arrow-key
// navigation between tabs, a matching tabpanel, and aria-controls wiring; these
// are links that change the URL, and a screen reader announcing "tab" for
// something that navigates is a worse lie than plain links are a limitation.
// aria-current="page" says the true thing.
export function MachineTabs({ id, active }: { id: string; active: string }) {
  return (
    <nav className="c-tabs" aria-label="Machine sections">
      {MACHINE_TABS.map((tab) => (
        <NavLink
          key={tab.key}
          to={machineTabPath(id, tab.key)}
          aria-current={tab.key === active ? 'page' : undefined}
          className={tab.key === active ? 'c-tab c-tab-active' : 'c-tab'}
        >
          {tab.label}
        </NavLink>
      ))}
    </nav>
  );
}
