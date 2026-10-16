import type { ReactNode, Ref } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';

import { CodeMirrorEditor } from '@/shared/ui/CodeMirrorEditor';
import type { CodeMirrorEditorHandle } from '@/shared/ui/CodeMirrorEditor';
import { createYamlLanguage } from '../lib/yamlLanguage';
import { createYamlLinter, YAML_ERROR_MARK_CLASS } from '../lib/yamlLint';

/** @public features/pipelines UI — the pipeline flow editor's raw-YAML tab. */
export interface YamlCodeEditorProps {
  /** The YAML document text. */
  code: string;
  /** Fires ~30ms after the last keystroke (via `CodeMirrorEditor`'s own debounce). */
  onChangeCode: (code: string) => void;
  disabled?: boolean;
  /** Read the current document before a mode change unmounts the editor. */
  ref?: Ref<CodeMirrorEditorHandle>;
}

/**
 * The pipeline flow editor's YAML tab: a `CodeMirrorEditor` with a YAML
 * validity linter. Ported from `apps/elitea-ui/src/[fsd]/features/
 * pipelines/yaml-editor/ui/YamlCodeEditor.jsx` (baseline, 69 lines).
 *
 * Content resets use the `code` prop instead of a separate Redux reset channel.
 * The optional ref forwards the existing shared editor handle.
 * `EditorPanel` reads its current document before switching to Flow.
 * This preserves edits while the shared 30ms change notification remains pending.
 *
 * Styling deviation forced by the same S1-E prop-surface trim:
 * `CodeMirrorEditor` accepts no `className`/`sx` passthrough (confirmed by
 * reading its prop list), so the baseline's
 * `styled(Field.CodeMirrorEditor)({'& .error_yaml_code': {...}})` wrapper
 * (which relied on `className` reaching the editor root) cannot be ported
 * as-is. The `.error_yaml_code` CSS class itself is still produced by CM6
 * (via `createYamlLinter`'s `markClass`) directly on the diagnostic's marked
 * DOM span, regardless of which component rendered it — so the same nested
 * `& .error_yaml_code` rule is applied one level up, on this component's own
 * wrapping `Box` (an ancestor of that span either way), producing the exact
 * same visual highlight. `className="nopan nodrag nowheel"` (baseline: React
 * Flow's "exclude this element from pan/zoom/wheel gestures" convention,
 * checked by React Flow via `closest()` against the event target's
 * ancestors) moves to this same wrapping `Box` for the identical reason.
 *
 * ADVERSARIAL-REVIEW FIXES (A2-fstring-yaml cluster), both scoped to this
 * file + its new `../lib/yamlLanguage.ts` sibling only:
 *
 * 1. Syntax highlighting was dropped, not just the linter's styling.
 *    `createYamlLinter`'s own doc comment already disclosed the gap: the
 *    baseline's colourised keys/strings/comments came from
 *    `StreamLanguage.define(yaml)` over `@codemirror/legacy-modes/mode/yaml`,
 *    and neither that package nor `@codemirror/lang-yaml` is installed —
 *    adding one is a toolchain-level call outside this slice's scope (the
 *    same call `features/toolkits/.../codeLanguageExtensions.ts` already
 *    made for the same reason). Rather than leave YAML unhighlighted,
 *    `../lib/yamlLanguage.ts` defines a small hand-rolled `StreamParser`
 *    using only `@codemirror/language`'s own `StreamLanguage`/`StringStream`
 *    (already installed — no new package): it colourises mapping keys,
 *    quoted/plain scalars, comments, booleans/null, numbers, list markers,
 *    anchors/aliases/tags and document markers, via the exact same
 *    `tags.propertyName/string/comment/atom/number/punctuation` buckets
 *    `CodeMirrorEditor.tsx`'s `highlightStyle` already styles — see that
 *    file's own doc comment for the token-name -> `@lezer/highlight` `tags`
 *    resolution this relies on, and for what it deliberately does not cover
 *    (multi-line block scalars, nested flow-mapping keys).
 *
 * 2. The error-mark background read fainter than the baseline
 *    (`rgba(215,22,22,0.20)` -> `background.errorBkg`'s 0.08 alpha, a 60%
 *    reduction). `background.errorBkg` is a shared design token
 *    (`shared/brand/tokens/default.pack.json`) used identically by
 *    `BannerMessage`/`CredentialWarningBanner` for whole-surface error
 *    banners — editing its alpha, or adding a new higher-alpha token
 *    variant, is a brand-tokens-package change that is OUT OF THIS
 *    CLUSTER'S SCOPE (that file is shared by every Wave-2 sub-unit in this
 *    worktree, and its two schemes are covered by their own contract test).
 *    A literal `rgba(215, 22, 22, 0.20)` re-hardcode is also not an
 *    available fix: `elitea/no-raw-color` (R-T1) bans raw colour
 *    literals/colour functions outside `shared/brand/tokens/`, and this app
 *    derives its palette per tenant (`shared/brand/color.ts#deriveColor`) —
 *    a hardcoded `(215, 22, 22)` triple would silently stop matching a
 *    re-hued tenant's actual error red, which the baseline's own single
 *    hardcoded value never had to account for. `@mui/material/styles`'
 *    `alpha()` cannot help either: it `decomposeColor()`s its input, which
 *    throws on a `var(--el-...)` string (verified against
 *    `node_modules/@mui/system/colorManipulator/colorManipulator.mjs`), and
 *    this app's `theme.vars.palette.*` tokens (R-T7 — required outside
 *    `shared/brand/`, `elitea/no-theme-palette`) are exactly such strings,
 *    not resolved literals. `color-mix()` is explicitly in `no-raw-color`'s
 *    banned-function list too. Within this scope, the best available fix is
 *    a second, independent visual cue: a solid `border.error`-coloured left
 *    border (`rgba(215,22,22,0.4)` in both schemes, already a vetted token,
 *    already used the same way elsewhere in this app — e.g.
 *    `shared/ui/Token/Token.tsx`'s `0.1875rem solid
 *    theme.vars.palette.border.lines` accent) stacked on the existing
 *    background wash. This restores a strong, unambiguous "this line is
 *    wrong" signal without a raw literal and without touching the shared
 *    token file. The literal 0.20-alpha-equivalent fix — a new
 *    `background.errorHighlight`-shaped token, or plumbing MUI's
 *    CSS-variable "channel" companions (`errorMainChannel`) through
 *    `toMuiPalette.ts`/`buildTheme.ts` so a component-local
 *    `rgba(var(--el-palette-error-mainChannel) / 0.2)` becomes possible —
 *    needs routing to whoever owns `shared/brand/`.
 */
export function YamlCodeEditor({ code, onChangeCode, disabled = false, ref }: YamlCodeEditorProps): ReactNode {
  const extensions = useMemo(() => [createYamlLanguage(), createYamlLinter()], []);

  return (
    <Box
      className="nopan nodrag nowheel"
      data-testid="pipeline-yaml-editor"
      sx={(theme) => ({
        width: '100%',
        maxWidth: '100%',
        height: '100%',
        display: 'flex',
        flexDirection: 'column',
        overflow: 'auto',
        // R-T1: colours come from brand tokens, never raw literals. The
        // baseline hardcoded `rgba(215, 22, 22, 0.20)`; `background.errorBkg`
        // (`default.pack.json`) is the same red already used brand-wide for
        // error surfaces, at that token's own 0.08 alpha (vs. baseline's
        // ad-hoc 0.20) — a live CSS-variable reference that repaints
        // correctly for both colour schemes (R-T7) instead of one fixed
        // literal for both, but visually fainter on its own. A solid
        // `border.error`-coloured left border (see the file doc comment's
        // fix-2 note for why this, and not a higher-alpha background, is
        // this cluster's fix) restores a strong, unambiguous "this line is
        // wrong" signal on top of it.
        [`& .${YAML_ERROR_MARK_CLASS}`]: {
          backgroundColor: theme.vars.palette.background.errorBkg,
          background: theme.vars.palette.background.errorBkg,
          borderLeft: `0.1875rem solid ${theme.vars.palette.border.error}`,
        },
      })}
    >
      <CodeMirrorEditor
        ref={ref ?? null}
        value={code}
        onChange={onChangeCode}
        extensions={extensions}
        height="100%"
        minHeight="400px"
        readOnly={disabled}
      />
    </Box>
  );
}
