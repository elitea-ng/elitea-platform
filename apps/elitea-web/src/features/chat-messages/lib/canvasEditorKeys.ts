/**
 * The canvas editor's own keyboard, as one rule the editor can be tested
 * against without a keyboard.
 *
 * ── WHY THE EDITOR BINDS KEYS AT ALL ──────────────────────────────────────
 * Undo and redo were toolbar buttons and nothing else. That is not "the same
 * thing, one click away": the header buttons drive whichever pane is mounted
 * through its imperative handle, and the pane that has no keymap of its own —
 * the TABLE grid, whose history is a snapshot list, not a document — could
 * therefore only be undone by taking a hand off the keyboard mid-edit.
 * `Mod-z` did nothing at all in a table canvas.
 *
 * ── AND WHY IT REFUSES TO ACT ON A KEY PRESSED INSIDE CODEMIRROR ──────────
 * The code pane already has `historyKeymap` (`shared/ui/CodeMirrorEditor`
 * installs it explicitly), bound as a NATIVE listener on the CodeMirror
 * element. A native listener on the target runs before React's own delegated
 * listener at the root container, so an editor-level handler that also undid
 * would undo TWICE on every press — one step further back than the reader
 * asked for, which is a worse failure than the missing binding, because it
 * silently discards an edit the reader still wanted.
 *
 * So `fromCodePane` is not a nicety: it is the difference between adding the
 * table's missing binding and breaking the code pane's working one. The
 * editor-level handler still covers the code pane whenever focus is anywhere
 * ELSE inside the editor — the header, the presence row, the diagram preview —
 * which is exactly where CodeMirror's own keymap cannot reach.
 *
 * ── ESCAPE ────────────────────────────────────────────────────────────────
 * Escape collapses full screen and NOTHING else. The editor is mounted in a
 * MUI drawer, and a drawer closes on Escape by default — which for this
 * drawer means SAVING and closing the canvas. Collapsing and closing on the
 * same key would make one press do two different things depending on a mode
 * the reader may not remember being in, so full screen swallows the press and
 * a normal-size editor lets the drawer have it.
 */

/** What one key press means to the editor. */
export type CanvasEditorKeyAction = 'undo' | 'redo' | 'exit-full-screen' | 'ignore';

export interface CanvasEditorKeyEvent {
  readonly key: string;
  readonly ctrlKey: boolean;
  readonly metaKey: boolean;
  readonly shiftKey: boolean;
}

export interface CanvasEditorKeyContext {
  /** True while the editor fills the viewport — the only state Escape means anything in. */
  readonly isFullScreen: boolean;
  /** True when the press happened inside the CodeMirror pane, which handles undo/redo itself. */
  readonly fromCodePane: boolean;
}

/**
 * `Mod` is Cmd on Apple platforms and Ctrl elsewhere, and both are accepted
 * everywhere rather than branching on the platform: a browser on a Mac with an
 * external PC keyboard sends Ctrl, and CodeMirror's own `historyKeymap` takes
 * both for the same reason.
 *
 * The three redo spellings are the three the platforms disagree on —
 * `Mod-Shift-z` (macOS, Linux), `Mod-y` (Windows) — and both are honoured on
 * all of them.
 */
export function canvasEditorKeyAction(event: CanvasEditorKeyEvent, context: CanvasEditorKeyContext): CanvasEditorKeyAction {
  if (event.key === 'Escape') return context.isFullScreen ? 'exit-full-screen' : 'ignore';
  const mod = event.ctrlKey || event.metaKey;
  if (!mod || context.fromCodePane) return 'ignore';
  const key = event.key.toLowerCase();
  if (key === 'y') return 'redo';
  if (key !== 'z') return 'ignore';
  return event.shiftKey ? 'redo' : 'undo';
}
