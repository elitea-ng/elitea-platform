import { EditorView } from '@codemirror/view';

/** The tallest the Instructions field grows before it scrolls; the full-screen button is for anything longer. */
export const INSTRUCTIONS_MAX_HEIGHT = '24rem';

/**
 * Makes the Instructions editor read as a text input rather than a code editor (#1979).
 *
 * `CodeMirrorEditor` defaults to a viewport-tall, line-numbered code surface
 * (`calc(100vh - 220px)`, about 550px for one line of text). The caller sets
 * `height="auto"` so the field grows with its content; this theme caps that
 * growth and hides the gutters, which would otherwise show line numbers beside
 * prose. Done as an extension, not as new `CodeMirrorEditor` props: that
 * component is at its §3.5 12-prop budget.
 */
export const instructionsSizingExtension = EditorView.theme({
  '&': { maxHeight: INSTRUCTIONS_MAX_HEIGHT },
  '.cm-scroller': { overflow: 'auto' },
  '.cm-gutters': { display: 'none' },
});
