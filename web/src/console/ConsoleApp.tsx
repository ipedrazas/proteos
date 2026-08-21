import { Navigate, Route, Routes, useNavigate, useParams } from 'react-router';
import type { Me } from '../api/client';
import { useMachineEvents } from '../api/hooks';
import { LaunchWizard } from './machines/LaunchWizard';
import { MachineDetail } from './machines/MachineDetail';
import { MachinesList } from './machines/MachinesList';
import { SideNav } from './nav';
import { MACHINES, machineOverviewPath } from './paths';
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
// MachineDetailRedirect sends /console/machines/:id to its first tab. It needs
// the id from the route, so it cannot be a bare <Navigate> in the route table.
function MachineDetailRedirect() {
  const { id } = useParams();
  return <Navigate to={id ? machineOverviewPath(id) : MACHINES} replace />;
}

export function ConsoleApp({ me }: { me: Me }) {
  const navigate = useNavigate();
  // One SSE subscription for the whole console, mounted here because every
  // console route wants live machine state and this is the lowest point that
  // covers them all. It must NOT move above the route split: the desktop and
  // mobile shells open their own stream, and only one shell is ever mounted.
  useMachineEvents(() => navigate('/login', { replace: true }));

  return (
    <div className="c-app">
      <TopBar me={me} />
      <div className="c-body">
        <SideNav isAdmin={me.is_admin} />
        <main className="c-main">
          <Routes>
            <Route index element={<Navigate to={MACHINES} replace />} />
            <Route path="machines" element={<MachinesList me={me} />} />
            {/* Ordered before machines/:id, though React Router ranks a static
                segment above a dynamic one regardless — MachinesList.test and
                ConsoleApp.test both pin it, because "launch" quietly becoming a
                machine id is exactly the kind of thing that would look like a
                missing machine rather than a routing bug. */}
            <Route path="machines/launch" element={<LaunchWizard me={me} />} />
            <Route path="machines/:id/overview" element={<MachineDetail me={me} />} />
            {/* The detail root redirects to its first tab, so /machines/:id is
                a usable link. Phase 3 adds the sibling tabs. */}
            <Route path="machines/:id" element={<MachineDetailRedirect />} />
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
