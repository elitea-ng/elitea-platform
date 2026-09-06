import type { ReactNode, Ref } from 'react';
import { useCallback, useEffect, useImperativeHandle, useMemo, useRef, useState } from 'react';

import { history, historyKeymap, redo, redoDepth, undo, undoDepth } from '@codemirror/commands';
import { syntaxHighlighting } from '@codemirror/language';
import { lintGutter } from '@codemirror/lint';
import { search, searchKeymap } from '@codemirror/search';
import { type Extension } from '@codemirror/state';
import { EditorView, keymap } from '@codemirror/view';
import { useTheme } from '@mui/material/styles';
import CodeMirror, { basicSetup, type ReactCodeMirrorRef } from '@uiw/react-codemirror';

import { buildEditorTheme, buildHighlightStyle } from './codeMirrorTheme';
import type { CodeMirrorSyntaxError } from './codeMirrorExtensions';
import { buildSyntaxErrorListener, createMaxLengthExtension } from './codeMirrorExtensions';

export type { CodeMirrorSyntaxError } from './codeMirrorExtensions';


/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface CodeMirrorEditorProps {
  /** Document text. */
  value: string;
  /** Fires ~30ms after the last keystroke (debounced, matching baseline's `notifyChange` — avoids flooding a form-level store on every keypress). */
  onChange?: (value: string) => void;
  /** Fires the current document text on blur. */
  onBlur?: (value: string) => void;
  /** Extra CodeMirror extensions — language support, a `linter()`, etc. */
  extensions?: Extension[];
  // Explicit `| undefined`: callers commonly forward an already-optional
  // `meta.disabled`-shaped value straight through (e.g.
  // `CommonStringField`'s code-language branch) — under
  // `exactOptionalPropertyTypes`, a plain `readOnly?: boolean` target
  // rejects that `boolean | undefined` value even though omitting the prop
  // entirely is fine. Same fix as `FieldHeaderProps`.
  readOnly?: boolean | undefined;
  /** Hard character cap; further input past this length is rejected in place. */
  maxLength?: number;
  height?: string;
  minHeight?: string;
  /** Diagnostics from any `linter()` extension in `extensions`, re-reported on every doc/viewport update. */
  onSyntaxError?: (errors: CodeMirrorSyntaxError[]) => void;
  'aria-label'?: string;
  /**
   * Undo/redo availability reporting, for a host that renders its OWN
   * history buttons outside this component (`features/chat-messages`'
   * `CanvasEditor`, through `CanvasEditHeader`). It cannot otherwise know
   * when to enable them — the history lives inside CodeMirror's state, and
   * `onChange` says nothing about depth.
   *
   * Grouped into one object rather than two sibling props for the §3.5
   * 12-prop component budget, the same grouping `ChatMessageList`'s
   * `messageActions`/`tts`/`continuation` use.
   */
  history?: CodeMirrorHistoryCallbacks;
  /** Imperative handle — see {@link CodeMirrorEditorHandle}. */
  ref?: Ref<CodeMirrorEditorHandle>;
}

/** Undo/redo availability callbacks — see {@link CodeMirrorEditorProps.history}. */
export interface CodeMirrorHistoryCallbacks {
  onCanUndo?: (canUndo: boolean) => void;
  onCanRedo?: (canRedo: boolean) => void;
}

/**
 * The imperative surface a host with its own toolbar needs.
 *
 * This was trimmed from the original port ("No imperative ref API") because
 * the two callers it shipped with — `ResizableCodeMirrorEditor` and
 * `CommonStringField` — attach no ref. That reasoning does not extend to
 * `CanvasEditor`, whose entire header row (undo, redo, copy, and the
 * remote-sync `setCode`) is driven through exactly these six members in the
 * baseline (`CanvasEditor.jsx:239-246,601-609`). Restored, additively: both
 * existing callers keep working unchanged, since every member is opt-in.
 */
export interface CodeMirrorEditorHandle {
  /** The current document text — read straight from CodeMirror's state, not the debounced mirror. */
  getCode: () => string;
  /** Replace the whole document (remote sync, quick-fix). Does NOT fire `onChange` — the caller already knows. */
  setCode: (next: string) => void;
  undo: () => void;
  redo: () => void;
  /** `@uiw/react-codemirror`'s own ref object, for anything not covered above. */
  readonly editor: HTMLDivElement | null;
  readonly view: ReactCodeMirrorRef['view'];
  readonly state: ReactCodeMirrorRef['state'];
}

const CHANGE_DEBOUNCE_MS = 30;

/**
 * A CodeMirror 6 text editor. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/field/CodeMirrorEditor.jsx` +
 * `.../shared/lib/hooks/useCodeMirror.hooks.js` — the baseline was already
 * CodeMirror 6 (`@uiw/react-codemirror@^4.23.10` over `codemirror@^6.0.1`,
 * same major line as this app's `@uiw/react-codemirror@4.25.11` / CM6
 * 6.37→6.43 packages), not the legacy CM5 imperative API, so this is a
 * same-major-version port, not a cross-major rewrite.
 *
 * Two real API-mismatch defects found while porting (exactly the class of
 * risk CodeMirror 6 was flagged as highest-risk for) and fixed here rather
 * than carried over:
 *
 * 1. **Baseline's `onSyntaxError` listener never actually read diagnostics.**
 *    It called `update.state.field(lintGutter, false)` — but `lintGutter` is
 *    the *extension-constructor function* exported by `@codemirror/lint`
 *    (`(config?) => Extension`), not a `StateField` instance.
 *    `EditorState.field()` requires an actual `StateField` object; hooking a
 *    function through with `required=false` returns `undefined` every time
 *    (verified against `node_modules/@codemirror/lint`'s `.d.ts` — the
 *    correct read API is the module's own exported
 *    `forEachDiagnostic(state, callback)`). The baseline's `onSyntaxError`
 *    callers therefore only ever received `[]`, silently. Fixed below by
 *    using `forEachDiagnostic` — and, once that read was fixed, a second,
 *    previously-invisible bug surfaced behind it: baseline's listener only
 *    re-runs on `docChanged || viewportChanged`, but `linter()` reports its
 *    (async, delayed) result via a separate `setDiagnosticsEffect`
 *    transaction that changes neither. See the inline comment at the
 *    listener below for how this is verified and fixed.
 * 2. **Baseline's `onBlur` forwarded the raw `FocusEvent`, not the edited
 *    value.** `CodeMirrorEditor.jsx` passes its `onBlur` prop straight
 *    through to `<CodeMirror onBlur={onBlur}>`, whose `onBlur` is a normal
 *    `React.FocusEventHandler<HTMLDivElement>` (`ReactCodeMirrorProps`
 *    extends `Omit<HTMLAttributes<HTMLDivElement>, 'onChange'|'placeholder'>`
 *    — `onBlur` is not one of the overridden keys). But baseline
 *    `CommonStringField.jsx` calls it as
 *    `onBlur={value => handleCodeFieldChange(fieldKey, value)}`, i.e. it
 *    expects the *string value*, not a `FocusEvent`. This component's
 *    `onBlur` prop is typed and implemented as `(value: string) => void` —
 *    the call site's actual expectation — by wrapping CodeMirror's native
 *    blur event internally instead of forwarding it.
 *
 * Scope trims versus the baseline (none of this unit's 9 in-scope call
 * sites — `ResizableCodeMirrorEditor`, `CommonStringField` — use any of
 * these):
 *  - RESTORED (was trimmed): the imperative ref API (`getCode`/`setCode`/
 *    `undo`/`redo`/`editor`/`view`/`state`) and `onCanUndo`/`onCanRedo`.
 *    The trim was justified by "neither in-scope caller ever attaches a
 *    ref" — true of `ResizableCodeMirrorEditor` and `CommonStringField`,
 *    and false of `features/chat-messages`' `CanvasEditor`, whose whole
 *    header row (undo/redo enablement, copy, remote-sync `setCode`) is
 *    driven through exactly these six members in the baseline
 *    (`CanvasEditor.jsx:239-246,601-609`). Both additions are opt-in, so
 *    the two ref-less callers are unchanged.
 *  - No `onKeyDown`/`autoHeight`/`maxHeight`/
 *    `width`/`minWidth`/`maxWidth`/`variant` props — unused by every
 *    in-scope caller; `variant` is fixed to `'bodyMedium'` (the baseline's
 *    own default, and the only value any in-scope caller ever needs).
 *  - No custom `foldByIndent` (baseline's
 *    `codeMirrorEditor.helpers.js#foldByIndent`, via `@codemirror/language`'s
 *    `foldService`) — `@codemirror/language` was not among the six
 *    `@codemirror/*` packages already pinned in `package.json` for this
 *    batch, and folding still works for JSON (this family's only in-scope
 *    language) via `@codemirror/lang-json`'s own syntax-tree fold info
 *    through `basicSetup({ foldGutter: true })`.
 *  - No `@uiw/codemirror-theme-vscode` (`vscodeDarkInit`/`vscodeLightInit`)
 *    — not an installed dependency, and baseline's `isDarkMode` JS branch to
 *    pick between the two is exactly what `elitea/no-mode-branch` (R-T2)
 *    bans app-wide. Colour theming below reads `theme.vars.palette.*`
 *    exclusively, which are live CSS-variable references (R-T7) — the
 *    editor repaints for the current colour scheme via the CSS cascade,
 *    with no JS mode branch and no extra dependency.
 *
 * Also fixed without being asked (found while porting, not a listed
 * baseline behaviour): baseline's `<CodeMirror>` never set its own
 * `basicSetup` prop, which defaults to `true` — i.e. it rendered the
 * *default* `basicSetup()` bundle AND `useCodeMirror`'s separately
 * hand-configured `basicSetup({ foldGutter: false, ... })`, registering the
 * whole basic-setup extension bundle (including a second copy of
 * `historyKeymap`/`searchKeymap`/etc.) twice. This port passes
 * `basicSetup={false}` to `<CodeMirror>` and supplies exactly one
 * `basicSetup(...)` call, configured explicitly (history/searchKeymap
 * disabled there since both are wired individually below, matching the
 * baseline's evident intent of customising them).
 */
export function CodeMirrorEditor({
  value,
  onChange,
  onBlur,
  extensions: extensionsProp,
  readOnly = false,
  maxLength = 0,
  height = 'calc(100vh - 220px)',
  minHeight = 'calc(100vh - 220px)',
  onSyntaxError,
  'aria-label': ariaLabel,
  history: historyCallbacks,
  ref,
}: CodeMirrorEditorProps): ReactNode {
  const theme = useTheme();
  const [code, setCode] = useState(value);
  /*
   * `ref` is taken as an ordinary prop rather than through `forwardRef`.
   * React 19 (this app pins 19.2.8) passes `ref` to function components
   * directly, and keeping this a plain function component is what leaves the
   * two existing callers — `ResizableCodeMirrorEditor` and
   * `CommonStringField`, neither of which attaches a ref — byte-for-byte
   * unaffected by this addition.
   */
  const cmRef = useRef<ReactCodeMirrorRef>(null);
  const lastNotifiedValueRef = useRef(value);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => {
    if (value !== lastNotifiedValueRef.current) {
      setCode(value);
      lastNotifiedValueRef.current = value;
    }
  }, [value]);

  useEffect(
    () => () => {
      if (debounceRef.current !== undefined) clearTimeout(debounceRef.current);
    },
    [],
  );

  const handleChange = useCallback(
    (newValue: string) => {
      setCode(newValue);
      if (debounceRef.current !== undefined) clearTimeout(debounceRef.current);
      debounceRef.current = setTimeout(() => {
        lastNotifiedValueRef.current = newValue;
        onChange?.(newValue);
      }, CHANGE_DEBOUNCE_MS);
    },
    [onChange],
  );

  const handleBlur = useCallback(() => {
    onBlur?.(code);
  }, [code, onBlur]);

  useImperativeHandle(
    ref,
    () => ({
      // Read from CodeMirror's own state, not the `code` mirror above: that
      // mirror is only written on change, so it is stale for anything
      // `setCode` or an external transaction put in the document.
      getCode: () => cmRef.current?.view?.state.doc.toString() ?? code,
      setCode: (next: string) => {
        const view = cmRef.current?.view;
        if (!view) return;
        view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: next } });
        // Keep the controlled mirror in step. `lastNotifiedValueRef` is moved
        // with it so the `value`-prop sync effect does not immediately undo
        // this write when the parent re-renders with its old `value`.
        setCode(next);
        lastNotifiedValueRef.current = next;
      },
      undo: () => {
        const view = cmRef.current?.view;
        if (view) undo(view);
      },
      redo: () => {
        const view = cmRef.current?.view;
        if (view) redo(view);
      },
      get editor() {
        return cmRef.current?.editor ?? null;
      },
      get view() {
        return cmRef.current?.view;
      },
      get state() {
        return cmRef.current?.state;
      },
    }),
    [code],
  );

  /*
   * Undo/redo availability, for a host rendering its own history buttons.
   *
   * `undoDepth`/`redoDepth` are the only honest source: a plain "has the doc
   * changed" flag says nothing about redo, and goes wrong the moment the user
   * undoes back to the original text. The listener is installed only when a
   * consumer asked for it, so the default path adds no extension.
   */
  const historyDepthListener = useMemo<Extension[]>(() => {
    const onCanUndo = historyCallbacks?.onCanUndo;
    const onCanRedo = historyCallbacks?.onCanRedo;
    if (!onCanUndo && !onCanRedo) return [];
    return [
      EditorView.updateListener.of((update) => {
        if (!update.docChanged && update.transactions.length === 0) return;
        onCanUndo?.(undoDepth(update.state) > 0);
        onCanRedo?.(redoDepth(update.state) > 0);
      }),
    ];
  }, [historyCallbacks]);

  const editorTheme = useMemo(() => buildEditorTheme(theme), [theme]);
  const highlightStyle = useMemo(() => buildHighlightStyle(theme), [theme]);

  const syntaxErrorListener = useMemo<Extension[]>(() => buildSyntaxErrorListener(onSyntaxError), [onSyntaxError]);

  const consumerExtensions = useMemo(() => extensionsProp ?? [], [extensionsProp]);

  // `aria-label` as a plain prop lands on @uiw/react-codemirror's *outer*
  // wrapper `<div>` (verified against its runtime source: every prop not in
  // its own excluded list is spread onto that div) — CM6's actual
  // `role="textbox"` element is the nested `.cm-content`, generated deeper
  // inside, and an ancestor `<div aria-label>` with no ARIA role does not
  // contribute to that element's accessible name. `EditorView.contentAttributes`
  // sets the attribute on `.cm-content` itself, which is where it is read.
  //
  // `tabindex` puts the document in the tab order, which is what makes the
  // scrolling region keyboard-reachable. CM6 builds
  // `.cm-editor > .cm-scroller > .cm-content`, sets `scrollDOM.tabIndex = -1`
  // itself, and marks `.cm-content` with `contenteditable` and nothing else —
  // so the element that SCROLLS has no tabbable descendant. That is what
  // `scrollable-region-focusable` (WCAG 2.1.1, ACT 0ssw9k) reports: axe counts
  // the natively focusable elements plus anything carrying a `tabindex`, and a
  // bare `div[contenteditable]` is neither (`axe-core`, `isNativelyFocusable`).
  // It stayed invisible while every checked editor was too short to scroll;
  // the pipeline starter template made the create screen's Instructions editor
  // overflow and six journeys failed on it. The attribute states what a
  // browser already does for an editable document, and ADDS the missing tab
  // stop for a `readOnly` one (`SettingsPreview`, `CodePreviewContent`,
  // `ArtifactPreviewContent`), where a keyboard user could not reach a
  // scrolled document at all. One tab stop, on the content and not on the
  // scroller, so the caret lands in the document and the arrows scroll it.
  const contentAttributeExtension = useMemo<Extension[]>(
    () => [
      EditorView.contentAttributes.of({
        tabindex: '0',
        ...(ariaLabel !== undefined ? { 'aria-label': ariaLabel } : {}),
      }),
    ],
    [ariaLabel],
  );

  return (
    <CodeMirror
      ref={cmRef}
      value={code}
      onChange={handleChange}
      onBlur={handleBlur}
      readOnly={readOnly}
      // NOT "light": @uiw/react-codemirror's built-in `'light'` base theme
      // hardcodes `background: #fff` on the editor root — a JS-level
      // constant, invisible to the CSS-variable scheme switch, and
      // independent of whatever `theme.vars.palette.*` colours the rest of
      // this component uses. Pairing that fixed white against
      // `text.primary` (`#A9B7C1` in this brand pack's light scheme, a
      // colour clearly tuned for a DARK surface) measured 2.05:1 —
      // Storybook's a11y addon caught it (`color-contrast`, needs 4.5:1).
      // `"none"` disables CM6's own base theme entirely, so `editorTheme`
      // below (built from the same `theme.vars.palette.*` tokens for both
      // background AND text) is the only colour source — background and
      // text always come from the same scheme-aware pair, with no JS mode
      // branch (R-T2) either.
      theme="none"
      basicSetup={false}
      height={height}
      minHeight={minHeight}
      extensions={[
        editorTheme,
        history(),
        keymap.of(historyKeymap),
        search(),
        keymap.of(searchKeymap),
        lintGutter(),
        basicSetup({
          lineNumbers: true,
          foldGutter: true,
          indentOnInput: true,
          highlightActiveLineGutter: true,
          searchKeymap: false,
          history: false,
          historyKeymap: false,
          // Replaced by the accessible `highlightStyle` above — leaving this
          // on would register CM6's own low-contrast `defaultHighlightStyle`
          // alongside it (both apply; the resulting DOM carries both sets of
          // highlight classes, whichever's stylesheet wins is unspecified).
          syntaxHighlighting: false,
        }),
        syntaxHighlighting(highlightStyle),
        EditorView.lineWrapping,
        ...contentAttributeExtension,
        ...syntaxErrorListener,
        ...historyDepthListener,
        ...consumerExtensions,
        createMaxLengthExtension(maxLength),
      ]}
    />
  );
}
