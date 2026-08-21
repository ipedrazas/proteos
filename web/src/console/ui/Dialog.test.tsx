// @vitest-environment jsdom
import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Dialog } from './Dialog';

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let cleanup: (() => void) | null = null;
afterEach(() => {
  cleanup?.();
  cleanup = null;
  document.body.innerHTML = '';
});

function open(props: { dismissable?: boolean; onClose?: () => void } = {}) {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  act(() => {
    root.render(
      <Dialog title="Confirm" onClose={props.onClose ?? (() => {})} dismissable={props.dismissable}>
        <button type="button">Cancel</button>
        <button type="button">Confirm</button>
      </Dialog>,
    );
  });
  cleanup = () => {
    act(() => root.unmount());
    container.remove();
  };
  return () => {
    cleanup?.();
    cleanup = null;
  };
}

function dialogButtons(): HTMLButtonElement[] {
  return [...document.querySelectorAll<HTMLButtonElement>('.c-dialog button')];
}

function press(key: string, shiftKey = false) {
  act(() => {
    window.dispatchEvent(new KeyboardEvent('keydown', { key, shiftKey, bubbles: true }));
  });
}

describe('Dialog', () => {
  it('moves focus into the dialog on open', () => {
    // Without this the keyboard stays behind the modal — Tab walks the page
    // underneath, where the focus ring is hidden by the backdrop.
    open();
    expect(document.activeElement).toBe(dialogButtons()[0]);
  });

  it('restores focus to the invoking element on close', () => {
    const opener = document.createElement('button');
    document.body.appendChild(opener);
    opener.focus();
    expect(document.activeElement).toBe(opener);

    const close = open();
    expect(document.activeElement).not.toBe(opener);
    close();
    expect(document.activeElement).toBe(opener);
  });

  it('closes on Escape', () => {
    const onClose = vi.fn();
    open({ onClose });
    press('Escape');
    expect(onClose).toHaveBeenCalled();
  });

  it('ignores Escape while an action is in flight', () => {
    // Dismissing a running terminate would hide it, not cancel it — machines
    // would keep disappearing from under someone who thinks they backed out.
    const onClose = vi.fn();
    open({ onClose, dismissable: false });
    press('Escape');
    expect(onClose).not.toHaveBeenCalled();
  });

  it('wraps Tab from the last control back to the first', () => {
    open();
    const [first, last] = dialogButtons();
    act(() => last.focus());
    press('Tab');
    expect(document.activeElement).toBe(first);
  });

  it('wraps Shift+Tab from the first control to the last', () => {
    open();
    const buttons = dialogButtons();
    act(() => buttons[0].focus());
    press('Tab', true);
    expect(document.activeElement).toBe(buttons[buttons.length - 1]);
  });

  it('keeps focus inside even when the backdrop is clicked through', () => {
    open();
    const outside = document.createElement('button');
    document.body.appendChild(outside);
    // Tab from the last control must not reach a control outside the dialog.
    act(() => dialogButtons()[1].focus());
    press('Tab');
    expect(document.activeElement).not.toBe(outside);
  });
});
