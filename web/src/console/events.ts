// events turns a machine event into a line a person can read.
//
// The raw row is a state machine's audit record — from_state, to_state, a type
// string, and an untyped payload whose shape depends on the type. Rendering it
// literally gives you "transition provisioning running", which is not wrong but
// is not a sentence either. This maps each shape onto what actually happened,
// and is separated out because the payload handling is the part that breaks
// silently when a new event type is added.

import type { AttributedEvent } from '../api/hooks';
import { describeState } from './machineState';
import type { MachineState } from '../api/client';

type EventTone = 'ok' | 'error' | 'pending' | 'muted';

export interface EventLine {
  text: string;
  tone: EventTone;
}

function str(payload: Record<string, unknown>, key: string): string {
  const v = payload?.[key];
  return typeof v === 'string' ? v : '';
}

// stateLabel reuses the badge vocabulary so a feed and a table never disagree
// about what a state is called.
function stateLabel(state: string | null): string {
  if (!state) return 'unknown';
  return describeState(state as MachineState).label;
}

export function describeEvent(event: AttributedEvent): EventLine {
  const payload = event.payload ?? {};

  switch (event.type) {
    case 'git.clone': {
      const ok = Boolean(payload.ok);
      const detail = str(payload, 'detail');
      return ok
        ? { text: 'Repository cloned', tone: 'ok' }
        : { text: detail ? `Clone failed: ${detail}` : 'Clone failed', tone: 'error' };
    }
    case 'git.push': {
      const ok = Boolean(payload.ok);
      const detail = str(payload, 'detail');
      return ok
        ? { text: 'Pushed to remote', tone: 'ok' }
        : { text: detail ? `Push failed: ${detail}` : 'Push failed', tone: 'error' };
    }
    case 'git.pr': {
      const number = payload.number;
      const project = str(payload, 'project');
      const where = project ? ` in ${project}` : '';
      return {
        text:
          typeof number === 'number'
            ? `Opened pull request #${number}${where}`
            : `Opened pull request${where}`,
        tone: 'ok',
      };
    }
    case 'error': {
      const reason = str(payload, 'reason');
      const base =
        event.from_state && event.to_state
          ? `${stateLabel(event.from_state)} → ${stateLabel(event.to_state)}`
          : 'Error';
      return { text: reason ? `${base}: ${reason}` : base, tone: 'error' };
    }
    default: {
      // transition, info, and anything added server-side that we do not know
      // about yet. A state pair is the most useful thing we can say; failing
      // that, say the raw type rather than rendering an empty row.
      if (event.from_state && event.to_state) {
        const to = describeState(event.to_state as MachineState);
        return {
          text: `${stateLabel(event.from_state)} → ${to.label}`,
          tone: to.tone === 'ok' ? 'ok' : to.busy ? 'pending' : 'muted',
        };
      }
      const reason = str(payload, 'reason');
      return { text: reason || event.type, tone: 'muted' };
    }
  }
}
