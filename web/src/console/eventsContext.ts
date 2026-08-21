import { createContext, useContext } from 'react';
import type { AttributedEvent } from '../api/hooks';

// The console subscribes to the machine-event stream once, in ConsoleApp, and
// shares the rolling window through this context.
//
// A context rather than each page calling useMachineEvents: that hook opens an
// EventSource, so a second caller would mean a second stream — two connections,
// duplicate events, and two independent 100-event windows that disagree.
export const ConsoleEventsContext = createContext<AttributedEvent[]>([]);

export function useConsoleEvents(): AttributedEvent[] {
  return useContext(ConsoleEventsContext);
}
