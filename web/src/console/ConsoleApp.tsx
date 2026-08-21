import { Navigate, Route, Routes } from 'react-router';
import type { Me } from '../api/client';
import { MACHINES, SideNav } from './nav';
import { Placeholder } from './Placeholder';
import { TopBar } from './TopBar';
import './console.css';

// ConsoleApp is the console shell: a resource-oriented UI that runs side by side
// with the desktop window manager rather than replacing it. Where the desktop
// presents ProteOS as a machine you sit at, the console presents it as a fleet
// you administer — a service nav, a list page per resource, detail pages with
// tabs, and full-page wizards for creation.
//
// It is mounted lazily at /console/* (see App.tsx) so desktop-only users never
// download it, and it shares the api/, lib/, and components/ layers with every
// other shell — only the presentation is new.
//
// Phase 0 ships the shell alone: every service route renders a Placeholder. The
// later phases replace them one at a time, in the order of
// plans/console-ui-implementation.md.
export function ConsoleApp({ me }: { me: Me }) {
  return (
    <div className="c-app">
      <TopBar me={me} />
      <div className="c-body">
        <SideNav isAdmin={me.is_admin} />
        <main className="c-main">
          <Routes>
            <Route index element={<Navigate to={MACHINES} replace />} />
            <Route
              path="machines/*"
              element={
                <Placeholder
                  title="Machines"
                  summary="Every machine you own, with its state, size, and lifecycle actions — start, stop, connect, and terminate."
                />
              }
            />
            <Route
              path="templates"
              element={
                <Placeholder
                  title="Templates"
                  summary="The catalog of machine images you can launch from, with each one's default size and the limits your overrides must stay within."
                />
              }
            />
            <Route
              path="keys"
              element={
                <Placeholder
                  title="SSH keys"
                  summary="The public keys authorised to log into your machines, the account-wide SSH switch, and the managed key your machines push to git hosts with."
                />
              }
            />
            <Route
              path="secrets"
              element={
                <Placeholder
                  title="Secrets"
                  summary="Write-only credentials: AI provider API keys, personal access tokens for the CLI, git host tokens, and your Claude subscription."
                />
              }
            />
            <Route
              path="activity"
              element={
                <Placeholder
                  title="Activity"
                  summary="Machine state transitions and errors across your fleet, alongside the application log."
                />
              }
            />
            {/* Admin is nav-gated on me.is_admin, but the route stays mounted for
                everyone: the server is the access check, and a non-admin who
                types the URL should get the page's own empty/403 state rather
                than a silent bounce that hides why nothing happened. */}
            <Route
              path="admin"
              element={
                <Placeholder
                  title="Admin"
                  summary="Fleet-wide totals and every machine across every user. Requires the proteos.admin role."
                />
              }
            />
            {/* An unknown console path is a typo, not a request for another
                shell — keep it inside the console. Absolute, like every
                redirect and link here: a relative target under a splat route
                resolves against the matched pathname, so "machines" from
                /console/nope becomes /console/nope/machines, which matches the
                splat again and redirects forever. */}
            <Route path="*" element={<Navigate to={MACHINES} replace />} />
          </Routes>
        </main>
      </div>
    </div>
  );
}
