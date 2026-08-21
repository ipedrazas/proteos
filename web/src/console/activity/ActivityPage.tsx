import { useState } from 'react';
import type { LogSource } from '../../api/client';
import { useLogs } from '../../api/hooks';
import { useConsoleEvents } from '../eventsContext';
import { EventFeed } from './EventFeed';

// ActivityPage is the fleet's two record types side by side.
//
// They are genuinely different and the page does not pretend otherwise:
// machine events are a live SSE window with no history behind them, while the
// application log is a server-side query with real history. Merging them into
// one stream would imply the events go back as far as the logs do.
export function ActivityPage() {
  const events = useConsoleEvents();
  const [source, setSource] = useState<LogSource | 'all'>('all');
  const logs = useLogs(source);
  const entries = logs.data?.entries ?? [];

  return (
    <div className="c-page">
      <header className="c-page-header">
        <h1 className="c-page-title">Activity</h1>
        <p className="c-muted c-page-sub">
          What your machines have been doing, and what the control plane logged.
        </p>
      </header>

      <div className="c-panel">
        <h2 className="c-section-title">Machine events</h2>
        <p className="c-muted">
          Live from the event stream. History starts when this page&apos;s session connected — there
          is no server-side backlog to page through.
        </p>
        <EventFeed
          events={events}
          showMachine
          emptyMessage="No machine events yet. State changes appear here as they happen."
        />
      </div>

      <div className="c-panel">
        <div className="c-switch-row">
          <div>
            <h2 className="c-section-title">Application log</h2>
            <p className="c-muted">
              The control plane&apos;s own request and lifecycle lines, plus warnings reported by
              browser sessions.
            </p>
          </div>
          <div className="c-switch-control">
            <select
              className="c-select"
              value={source}
              onChange={(e) => setSource(e.target.value as LogSource | 'all')}
              aria-label="Filter log source"
            >
              <option value="all">All sources</option>
              <option value="api">Control plane</option>
              <option value="ui">Browser</option>
            </select>
          </div>
        </div>

        {logs.isLoading && <p className="c-muted c-keys-empty">Loading…</p>}
        {logs.isError && (
          <p className="c-banner c-banner-error" role="alert">
            Could not load the log.
          </p>
        )}
        {!logs.isLoading && entries.length === 0 && (
          <p className="c-muted c-keys-empty">Nothing logged.</p>
        )}

        {entries.length > 0 && (
          <ul className="c-feed c-log-feed">
            {entries.map((entry, i) => (
              <li className="c-feed-row" key={`${entry.time}-${i}`}>
                <time className="c-feed-time c-muted" dateTime={entry.time}>
                  {new Date(entry.time).toLocaleTimeString()}
                </time>
                <span className={`c-log-level c-log-${entry.level.toLowerCase()}`}>
                  {entry.level}
                </span>
                <span className="c-muted c-log-source">{entry.source}</span>
                <span className="c-feed-text">{entry.message}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
