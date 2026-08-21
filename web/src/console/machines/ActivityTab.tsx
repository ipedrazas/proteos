import type { MachineSummary } from '../../api/client';
import { EventFeed } from '../activity/EventFeed';
import { useConsoleEvents } from '../eventsContext';

// ActivityTab shows this machine's events.
//
// The feed is the live SSE window, not a history query — the control plane has
// no per-machine events endpoint, and event rows carry no machine column, so
// attribution only exists for events received while this browser session has
// been connected. The empty state says exactly that instead of implying the
// machine has been quiet.
export function ActivityTab({ machine }: { machine: MachineSummary }) {
  const events = useConsoleEvents().filter((e) => e.machine_id === machine.id);

  return (
    <div className="c-panel">
      <h2 className="c-section-title">Recent activity</h2>
      <EventFeed
        events={events}
        showMachine={false}
        emptyMessage="Nothing since this page was opened. State changes appear here as they happen."
      />
    </div>
  );
}
