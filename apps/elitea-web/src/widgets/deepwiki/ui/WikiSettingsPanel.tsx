/**
 * Edit the toolkit's settings as JSON and save them (DWIKI-010).
 *
 * VALIDATION IS `parseSettingsDraft`, not this component. It returns every
 * problem with the field it belongs to, and the one check the legacy screen
 * never made — that a repository is configured at all — is what stops a
 * generation from running and finding nothing.
 *
 * THE MODEL SETTINGS ARE ANNOUNCED, NOT ENFORCED. `llm_model` and
 * `embedding_model` name models the engine asks the platform gateway for, and
 * the gateway resolves models per project: a toolkit that names none falls back
 * to the engine's hardcoded defaults, and a project without those rows answers
 * 404 — the generation then "completes" with no pages, and only the gateway log
 * says why (measured 2026-09-02, PR #725). Save is not blocked, because the
 * document is legal and works wherever the project does resolve the fallback.
 *
 * THE WHOLE TOOLKIT IS SENT. The route is a PUT and replaces the resource;
 * `useSaveWikiSettings` spreads the toolkit as last read and overwrites only
 * `settings`, which is why this panel needs the raw row and not just the
 * settings.
 */
import { useMemo, useRef, useState } from 'react';

import Alert from '@mui/material/Alert';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';
import { json } from '@codemirror/lang-json';

import { getArtifactSource, type ToolkitSettings } from '@/entities/wiki';
import { canSaveSettings, parseSettingsDraft, useSaveWikiSettings, withArtifactSource, type SettingsHint } from '@/features/wiki-settings';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { CodeMirrorEditor, type CodeMirrorEditorHandle } from '@/shared/ui/CodeMirrorEditor';

import { WikiSourcePicker, type WikiFolderSource } from './WikiSourcePicker';

/** Room for a typical toolkit document without scrolling; the editor grows with the draft. */
const SETTINGS_EDITOR_MIN_HEIGHT = '12.5rem';
/** Bounded: left to grow, the editor took the whole viewport and pushed the wiki below the fold (the settings shot). */
const SETTINGS_EDITOR_HEIGHT = '16rem';

/**
 * The hint's copy, per field. Two keys rather than one interpolated sentence:
 * a single generic "{{field}} is missing" reads as a lint rule, and the thing
 * an operator needs is WHICH call breaks and what it asks for.
 */
function hintMessage(hint: SettingsHint): string {
  if (hint.field === 'embedding_model') {
    return t(
      'deepwiki.settings.hint.embeddingModel',
      'No embedding_model is set, so the engine asks this project for {{fallback}}. Models resolve per project: if this project has no such model, indexing is refused and the generation finishes with no pages.',
      { fallback: hint.fallback },
    );
  }
  return t(
    'deepwiki.settings.hint.llmModel',
    'No llm_model is set, so generation and wiki chat ask this project for {{fallback}}. Models resolve per project: if this project has no such model, every call is refused and the generation finishes with no pages.',
    { fallback: hint.fallback },
  );
}

interface WikiSettingsPanelProps {
  readonly projectId: string | number;
  readonly toolkitId: string | number;
  /** The toolkit row as last read. */
  readonly toolkit: Record<string, unknown>;
  readonly settings: ToolkitSettings;
}

export function WikiSettingsPanel({ projectId, toolkitId, toolkit, settings }: WikiSettingsPanelProps): React.JSX.Element {
  const [draft, setDraft] = useState(() => JSON.stringify(settings, null, 2));
  const [saved, setSaved] = useState(false);
  const save = useSaveWikiSettings();
  const parsed = useMemo(() => parseSettingsDraft(draft), [draft]);
  // Read at Save time straight from CodeMirror: `draft` is the editor's
  // DEBOUNCED mirror (30ms), and a Save pressed inside that window saved the
  // previous document — the journey caught a good save where a refusal was
  // due, because the refused text had not reached `draft` yet.
  const editorRef = useRef<CodeMirrorEditorHandle>(null);
  const extensions = useMemo(() => [json()], []);
  /*
   * The text this panel last wrote INTO the editor.
   *
   * CodeMirror reports a programmatic write back through the same debounced
   * `onChange` an operator's keystroke uses, 30ms later. Read as a new edit,
   * that echo cleared the "Settings saved" notice a moment after a save made
   * from the picker — the save succeeded and the screen said nothing about
   * it. The ref is how an edit is told apart from an echo.
   */
  const writtenRef = useRef<string | null>(null);

  // The picker rewrites the DOCUMENT THE EDITOR HOLDS, not the debounced
  // mirror: the same 30ms window that made Save judge the previous draft
  // would make a bucket choice discard the last thing typed.
  const applySource = (source: WikiFolderSource | null): void => {
    const current = editorRef.current?.getCode() ?? draft;
    const now = current === draft ? parsed : parseSettingsDraft(current);
    if (now.settings === null) return;
    const next = JSON.stringify(withArtifactSource(now.settings, source), null, 2);
    writtenRef.current = next;
    editorRef.current?.setCode(next);
    setDraft(next);
    setSaved(false);
  };

  const acceptDraft = (next: string): void => {
    setDraft(next);
    if (next === writtenRef.current) return;
    setSaved(false);
  };

  return (
    <Stack sx={{ gap: 1 }} data-testid="wiki-settings-panel">
      <Typography variant="headingSmall">{t('deepwiki.settings.title', 'Toolkit settings')}</Typography>
      <WikiSourcePicker
        projectId={projectId}
        source={getArtifactSource(settings)}
        disabled={parsed.settings === null}
        onChange={applySource}
      />
      <CodeMirrorEditor ref={editorRef} value={draft} onChange={acceptDraft} extensions={extensions} minHeight={SETTINGS_EDITOR_MIN_HEIGHT} height={SETTINGS_EDITOR_HEIGHT} />

      {parsed.problems.map((problem) => (
        <Alert key={`${problem.field ?? 'document'}:${problem.message}`} severity="warning" data-testid="wiki-settings-problem" data-field={problem.field ?? ''}>
          {problem.field === null ? problem.message : `${problem.field}: ${problem.message}`}
        </Alert>
      ))}

      {parsed.hints.map((hint) => (
        <Alert key={hint.field} severity="info" data-testid="wiki-settings-hint" data-field={hint.field}>
          {hintMessage(hint)}
        </Alert>
      ))}

      {save.isError ? (
        <Alert severity="error" data-testid="wiki-settings-error">
          {t('deepwiki.settings.saveFailed', 'The settings were not saved: {{reason}}', { reason: save.error.message })}
        </Alert>
      ) : null}
      {saved && save.isSuccess ? (
        <Alert severity="success" data-testid="wiki-settings-saved">
          {t('deepwiki.settings.saved', 'Settings saved. The next generation will use them.')}
        </Alert>
      ) : null}

      <Stack sx={{ flexDirection: 'row', gap: 1 }}>
        <BaseBtn
          variant="elitea"
          disabled={!canSaveSettings(parsed)}
          loading={save.isPending}
          data-testid="wiki-settings-save"
          onClick={() => {
            const current = editorRef.current?.getCode() ?? draft;
            const now = current === draft ? parsed : parseSettingsDraft(current);
            if (current !== draft) {
              setDraft(current);
              setSaved(false);
            }
            if (!canSaveSettings(now) || now.settings === null) return;
            save.mutate(
              { projectId, toolkitId, toolkit, settings: now.settings },
              { onSuccess: () => { setSaved(true); } },
            );
          }}
        >
          {t('deepwiki.settings.save', 'Save settings')}
        </BaseBtn>
      </Stack>
    </Stack>
  );
}
