/**
 * Replaces xterm.js (`@xterm/xterm`, `@xterm/addon-fit`) with a recorder, so a
 * `TerminalView` test reads what was written and types into it without a
 * canvas (R-M1: `vi.mock` lives only under `__mocks__/`).
 *
 * Import this module BEFORE the module under test.
 */
import { vi } from 'vitest';

export interface FakeTerminal {
  written: string[];
  type: (data: string) => void;
}

/** Every terminal opened since the last reset, oldest first. */
export const terminals: FakeTerminal[] = [];

vi.mock('@xterm/xterm/css/xterm.css', () => ({}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit(): void {}
  },
}));
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    rows = 24;
    cols = 80;
    options: Record<string, unknown> = {};
    private readonly written: string[] = [];
    private onInput: (data: string) => void = () => undefined;
    constructor() {
      terminals.push({ written: this.written, type: (data) => this.onInput(data) });
    }
    loadAddon(): void {}
    open(): void {}
    write(data: string): void {
      this.written.push(data);
    }
    onData(handler: (data: string) => void): { dispose: () => void } {
      this.onInput = handler;
      return { dispose: () => undefined };
    }
    dispose(): void {}
  },
}));
