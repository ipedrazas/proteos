import type { AttributedEvent } from '../../api/hooks';
import { describeEvent } from '../events';

// EventFeed renders machine events newest-first. Shared by the per-machine
// Activity tab and the fleet-wide Activity page, which differ only in whether
// the machine name is worth a column.
export function EventFeed({
  events,
  showMachine,
  emptyMessage,
}: {
  events: AttributedEvent[];
  showMachine: boolean;
  emptyMessage: string;
}) {
  if (events.length === 0) return <p className="c-muted c-keys-empty">{emptyMessage}</p>;

  return (
    <ul className="c-feed">
      {events.map((event) => {
        const line = describeEvent(event);
        return (
          <li className="c-feed-row" key={event.id}>
            <time className="c-feed-time c-muted" dateTime={event.created_at}>
              {new Date(event.created_at).toLocaleTimeString()}
            </time>
            {showMachine && (
              <span className="c-feed-machine">
                {event.machine_name ?? <span className="c-muted">—</span>}
              </span>
            )}
            <span className={`c-feed-text c-feed-${line.tone}`}>{line.text}</span>
          </li>
        );
      })}
    </ul>
  );
}
