/**
 * The CodeMirror colour theme and syntax-highlight style, split out of
 * `CodeMirrorEditor.tsx` to keep that file under the §3.5 400-line budget.
 * Pure builders over the MUI theme — no React, no hooks; the component
 * memoises the two calls itself.
 *
 * Every colour is a `theme.vars.palette.*` CSS-variable reference (R-T7), so
 * the editor repaints for the viewer's colour scheme through the cascade,
 * with no JS mode branch (R-T2).
 */
import { HighlightStyle } from '@codemirror/language';
import type { Extension } from '@codemirror/state';
import { EditorView } from '@codemirror/view';
import { tags } from '@lezer/highlight';
import type { Theme } from '@mui/material/styles';

export function buildEditorTheme(theme: Theme): Extension {
    const typography = theme.typography.bodyMedium;
    // `TypographyVariants['bodyMedium']` is a plain `CSSProperties`, so every
    // member is optional (`string | number | undefined`); CM6's `StyleSpec`
    // rejects `undefined` outright (only `string | number | StyleSpec |
    // null`). A fallback at the end of each chain resolves every member to a
    // concrete `string`/`number` for the type checker, not just at runtime.
    // `theme.typography.fontFamily` is itself typed `CSSProperties['fontFamily']`
    // (MUI's `createTypography.d.ts`), i.e. `string | undefined` too — a
    // real theme always sets it, but the type checker doesn't know that, so
    // the fallback chain needs a literal at the end to land on `string`.
    const fontFamily: string = typography.fontFamily ?? theme.typography.fontFamily ?? 'inherit';
    const lineHeight: string | number = typography.lineHeight ?? 1.5;
    return EditorView.theme({
      '&': {
        backgroundColor: theme.vars.palette.background.codeMirrorEditor,
      },
      '.cm-content': {
        fontFamily,
        fontSize: theme.typography.bodyMedium.fontSize ?? 'inherit',
        lineHeight,
        // Base colour for any character `highlightStyle` below doesn't tag
        // (whitespace, unmatched punctuation) — without this, `theme="none"`
        // (see the `<CodeMirror>` prop below) leaves it at the browser
        // default instead of a token paired with this same background.
        color: theme.vars.palette.text.primary,
        caretColor: theme.vars.palette.text.primary,
      },
      '.cm-gutters': {
        backgroundColor: theme.vars.palette.background.tabPanel,
        color: theme.vars.palette.text.secondary,
        border: 'none',
      },
      '.cm-activeLine, .cm-activeLineGutter': {
        backgroundColor: theme.vars.palette.background.tabPanel,
      },
      '.cm-editor': {
        borderRadius: theme.vars.shape.radiusSm,
      },
      '&.cm-focused': {
        outline: 'none',
      },
    });
}

  // Neither `<CodeMirror theme="light">` (CM6's own bundled light base — it
  // only sets background/selection/cursor colours, no syntax colours) nor
  // `basicSetup`'s `defaultHighlightStyle` fallback (from `@codemirror/
  // language`, pulled in automatically since no other highlight style is
  // registered) supply an accessible token palette: `defaultHighlightStyle`'s
  // string colour (`#a9b7c1`) is tuned for a dark editor background and
  // measures 2.05:1 against this app's light `background.codeMirrorEditor` —
  // Storybook's a11y addon (`a11y.test: 'error'`) failed the `WithJsonLinter`
  // story on exactly this (`color-contrast`, WCAG requires 4.5:1 for 14px
  // text). Two brand tokens already vetted elsewhere in this codebase for AA
  // text contrast (`text.primary`/`text.secondary`, same pair `HeadingChip`/
  // `CharacterCounter` use) replace it — flatter than a full syntax palette,
  // but every token here is a CSS-variable reference (R-T7), so it repaints
  // correctly for both colour schemes with no JS mode branch (R-T2).
export function buildHighlightStyle(theme: Theme): HighlightStyle {
  return HighlightStyle.define([
        { tag: [tags.propertyName, tags.keyword, tags.atom, tags.bool, tags.null], color: theme.vars.palette.text.primary, fontWeight: 600 },
        { tag: [tags.string, tags.number], color: theme.vars.palette.text.primary },
        { tag: [tags.comment], color: theme.vars.palette.text.secondary, fontStyle: 'italic' },
        { tag: [tags.punctuation, tags.bracket, tags.separator], color: theme.vars.palette.text.secondary },
  ]);
}

