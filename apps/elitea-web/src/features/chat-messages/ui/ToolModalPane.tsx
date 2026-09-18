/**
 * One half of `ToolModal`'s INPUT | OUTPUT split: a header row (caption +
 * language `Select` + copy button) over a read-only `CodeMirrorEditor`.
 *
 * Ported from the two mirror-image `Box` subtrees of
 * `apps/elitea-ui/src/components/Chat/ToolModal.jsx:236-373` (they differ
 * only in caption/value/handlers), collapsed into one component so
 * `ToolModal.tsx` stays inside the §3.5 file-length and complexity budgets.
 *
 * Deviations from the baseline, following the precedent
 * `shared/ui/ExpandedViewerModal/ExpandedViewerModal.tsx:125-132` set:
 *  - The baseline's `CodeMirrorEditorHelpers.languageOptions` (a ~40-entry
 *    language list) and `CodeMirrorLinterHelpers.getExtensionsByLang`
 *    (dynamic `import()` per language) were deliberately NOT ported to this
 *    app, and are not resurrected here. `@codemirror/lang-json` is the only
 *    CodeMirror language package this app installs (checked live against
 *    `package.json`), so the option list below is exactly what this app can
 *    actually render, and the selector is a plain MUI `Select` driven by
 *    that list — the same shape `ExpandedViewerModal.LanguageSelect` uses.
 *  - `SingleSelect`/`CopyIcon` (legacy wrappers, not in `shared/ui`) are
 *    plain MUI `Select` / `@mui/icons-material/ContentCopy`, matching
 *    `ExpandedViewerModal`'s own interim-icon rationale.
 *  - No `react-split` draggable gutter — the split is a CSS grid (adding a
 *    dependency is a toolchain-level decision, spec §2.5).
 */
import type { ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import { json } from '@codemirror/lang-json';
import type { Extension } from '@codemirror/state';
import { EditorView } from '@codemirror/view';

import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import Box from '@mui/material/Box';
import FormControl from '@mui/material/FormControl';
import IconButton from '@mui/material/IconButton';
import MenuItem from '@mui/material/MenuItem';
import Select, { type SelectChangeEvent } from '@mui/material/Select';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { CodeMirrorEditor } from '@/shared/ui/CodeMirrorEditor';
import { t } from '@/shared/i18n';

/** The baseline's first format option, kept verbatim (`ToolModal.jsx:27`). */
const TOOL_MODAL_AUTO_LANGUAGE = 'auto';

/** @public One entry of {@link useToolModalLanguageOptions}. */
export interface ToolModalLanguageOption {
  readonly value: string;
  readonly label: string;
}

/**
 * `'auto'` first (baseline parity), then the languages this app can actually
 * highlight. Deliberately short — see the file doc comment.
 */
function useToolModalLanguageOptions(): readonly ToolModalLanguageOption[] {
  return useMemo(
    () => [
      { value: TOOL_MODAL_AUTO_LANGUAGE, label: t('chatMessages.toolModal.languageAuto', 'Auto-detect') },
      { value: 'json', label: t('chatMessages.toolModal.languageJson', 'JSON') },
      { value: 'text', label: t('chatMessages.toolModal.languageText', 'Text') },
    ],
    [],
  );
}

/**
 * Resolves `'auto'` against the content itself: JSON when the text parses as
 * JSON, plain text otherwise. This is NOT the baseline's
 * `detectContentType` (a multi-format sniffer for a language list this app
 * does not have) — it distinguishes only between the two outcomes this app
 * can render differently.
 */
export function resolveToolModalLanguage(selected: string, content: string): string {
  if (selected !== TOOL_MODAL_AUTO_LANGUAGE) return selected;
  const trimmed = content.trim();
  if (trimmed.startsWith('{') || trimmed.startsWith('[')) {
    try {
      JSON.parse(trimmed);
      return 'json';
    } catch {
      return 'text';
    }
  }
  return 'text';
}

/**
 * Read-only view: syntax highlighting only, no `linter()` — a lint gutter
 * marking someone else's tool output as "invalid" is noise the user cannot
 * act on (the baseline uses its `extensionWithoutLinter` bundle here for the
 * same reason).
 */
function toolModalExtensions(language: string): Extension[] {
  return language === 'json' ? [json(), EditorView.lineWrapping] : [EditorView.lineWrapping];
}

/** @public Props for {@link ToolModalPane}. */
export interface ToolModalPaneProps {
  /** Pane caption — `INPUT` / `OUTPUT`. */
  readonly caption: string;
  /** Document text shown in the read-only editor. */
  readonly value: string;
  /** Distinguishes the two panes' controls for assistive tech and tests. */
  readonly paneId: string;
}

const paneStyles = {
  root: {
    display: 'flex',
    flexDirection: 'column',
    minWidth: 0,
    minHeight: '9.375rem',
    overflow: 'hidden',
  },
  header: (theme: Theme) => ({
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    gap: theme.spacing(1),
    padding: theme.spacing(1.5, 3),
    backgroundColor: theme.vars.palette.background.tabPanel,
    borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
    minHeight: '3.25rem',
  }),
  headerLeft: (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    gap: theme.spacing(1),
    minWidth: 0,
  }),
  body: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column',
    minHeight: 0,
    overflow: 'auto',
  },
} as const;

/** One INPUT/OUTPUT pane of `ToolModal`. */
export function ToolModalPane({ caption, value, paneId }: ToolModalPaneProps): ReactNode {
  const options = useToolModalLanguageOptions();
  const [language, setLanguage] = useState<string>(TOOL_MODAL_AUTO_LANGUAGE);

  const handleLanguageChange = useCallback((event: SelectChangeEvent<string>): void => {
    setLanguage(event.target.value);
  }, []);

  const handleCopy = useCallback((): void => {
    void navigator.clipboard?.writeText(value);
  }, [value]);

  const resolved = useMemo(() => resolveToolModalLanguage(language, value), [language, value]);
  const extensions = useMemo(() => toolModalExtensions(resolved), [resolved]);

  const languageLabel = t('chatMessages.toolModal.languageAriaLabel', 'Content type for {{pane}}', { pane: caption });
  const copyLabel = t('chatMessages.toolModal.copyAriaLabel', 'Copy {{pane}}', { pane: caption });

  return (
    <Box sx={paneStyles.root} data-testid={`tool-modal-pane-${paneId}`}>
      <Box sx={paneStyles.header}>
        <Box sx={paneStyles.headerLeft}>
          <Typography
            variant="bodyMedium"
            color="text.primary"
          >
            {caption}
          </Typography>
          <FormControl
            variant="standard"
            size="small"
            sx={{ minWidth: '7rem' }}
          >
            <Select<string>
              inputProps={{ 'aria-label': languageLabel, 'data-testid': `tool-modal-language-${paneId}` }}
              value={language}
              onChange={handleLanguageChange}
            >
              {options.map((option) => (
                <MenuItem
                  key={option.value}
                  value={option.value}
                >
                  {option.label}
                </MenuItem>
              ))}
            </Select>
          </FormControl>
        </Box>
        <Tooltip
          title={copyLabel}
          placement="top"
        >
          <IconButton
            aria-label={copyLabel}
            size="small"
            onClick={handleCopy}
          >
            <ContentCopyIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      </Box>
      <Box sx={paneStyles.body}>
        <CodeMirrorEditor
          value={value}
          readOnly
          extensions={extensions}
          height="100%"
          minHeight="100%"
          aria-label={caption}
        />
      </Box>
    </Box>
  );
}
