import type { KeyboardEvent, ReactNode } from 'react';
import { useCallback, useRef, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import type { SxProps, Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { ApplicationCreatedResponse } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import { useGenerateAgentDraftMutation } from '../../api/generateAgentDraft';
import { applicationErrorMessage } from '../../lib/errorMessage';
import { EMPTY_AGENT_DRAFT, mapApplicationDraft, type AgentDraft } from '../../lib/agentDraft';
import { GenerateAgentReviewForm } from './GenerateAgentReviewForm';
import { useAgentDraftApproval } from './useAgentDraftApproval';
import { useToggleSet } from './useToggleSet';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent/ui/generate-agent-modal/GenerateAgentModal.jsx`,
 * merged with the entity-agnostic step machine of `apps/elitea-ui/src/[fsd]/
 * entities/generate-entity-with-ai/ui/GenerateEntityModal.jsx`.
 *
 * **DISCLOSED REDESIGN — the generic `entities/generate-entity-with-ai`
 * layer was not promoted.** The Wave-2 promotion pass promoted exactly two
 * slices (`entities/application-form`, `entities/toolkit` — see this
 * sub-unit's own mission brief). `entities/generate-entity-with-ai`
 * (`GenerateEntityButton`/`GenerateEntityModal`) was NOT one of them, and
 * this sub-unit has exactly one consumer for that generic step machine
 * (agents) — duplicating an entities-layer abstraction nobody else can
 * reach yet (features/ may not create a NEW entities/ slice; that is a
 * cross-cutting promotion decision outside this sub-unit's ownership
 * fence) would just be dead generality. Every state/step the baseline's
 * `GenerateEntityModal` implements (`input` → `loading` → `review`,
 * abort-on-close, back-to-prompt) is preserved, inlined into this
 * agent-specific component instead of a separate generic wrapper.
 *
 * **abort-on-close, one disclosed deviation.** The baseline's
 * `generatePromiseRef.current.abort()` cancels the underlying RTK Query
 * mutation-trigger promise itself. `../../api/generateAgentDraft.ts`'s
 * `useGenerateAgentDraftMutation` (outside this sub-unit's file scope) wraps
 * a bare `queryClient.query` and exposes no abort/cancel handle, so a
 * literal network-level abort is not reachable from this component today —
 * see that file for the real fix (thread an `AbortController`/expose a
 * cancel fn, or call `queryClient.cancelQueries` against the query key
 * `getGenerateAgentDraftQueryOptions` produces). What IS implemented here,
 * inside this file's own scope: `handleClose` bumps `generationTokenRef`,
 * and `handleGenerate` captures the token before awaiting and checks it
 * after — so a generate request that resolves after the modal was closed
 * is discarded silently (no stale `draft`/`step`/error write-back), which
 * is the user-visible half of "abort-on-close" (closing never reopens the
 * modal into content from a request you already walked away from), even
 * though the HTTP request itself keeps running to completion in the
 * background.
 *
 * `handleGenerate` calls the served endpoint through
 * `../../api/generateAgentDraft.ts`'s `useGenerateAgentDraftMutation` and
 * maps its `ApplicationDraft` with `mapApplicationDraft`, so name,
 * description, instructions, welcome message and conversation starters all
 * arrive filled in (#254 P1). The five `suggested_*` lists stay empty: the
 * contract does not carry them — see `../../lib/agentDraft.ts` for why that
 * is the endpoint's answer and not this component's guess.
 *
 * **No-toast-system-yet convention** (`features/notifications/lib/
 * errorMessage.ts`'s own doc comment: dependency-inject the error sink,
 * `console.error`/`console.warn` fallback) applies to `onApproveError`
 * (approve-step failures) exactly as it does to `useAgentDraftApproval`'s
 * own `onAssociationWarning`.
 */

type Step = 'input' | 'loading' | 'review';

export interface GenerateAgentModalProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly projectId: string | undefined;
  readonly onAgentCreated: (result: ApplicationCreatedResponse) => void;
  /** Optional — falls back to `console.error` per the no-toast-system-yet convention (see module doc comment). */
  readonly onApproveError?: ((message: string) => void) | undefined;
  /** Forwarded to `useAgentDraftApproval` — see that hook's own doc comment. */
  readonly onAssociationWarning?: ((message: string) => void) | undefined;
}

function generateErrorMessage(error: unknown): string {
  return applicationErrorMessage(error) || t('features.agents.generateAgentModal.generateFailed', 'Failed to generate. Please try again.');
}

export function GenerateAgentModal({
  open,
  onClose,
  projectId,
  onAgentCreated,
  onApproveError,
  onAssociationWarning,
}: GenerateAgentModalProps): ReactNode {
  const { approve, isApproving } = useAgentDraftApproval({ projectId, onAssociationWarning });
  const { generateDraft, error: generateError, reset: resetGenerateError } = useGenerateAgentDraftMutation();

  const [step, setStep] = useState<Step>('input');
  const [description, setDescription] = useState('');
  const [draft, setDraft] = useState<AgentDraft>(EMPTY_AGENT_DRAFT);
  const [isDraftValid, setIsDraftValid] = useState(true);

  // Bumped by `handleClose`; `handleGenerate` captures the value before
  // awaiting `generateDraft` and compares it after — see the module doc
  // comment's "abort-on-close, one disclosed deviation" section.
  const generationTokenRef = useRef(0);

  const toolkits = useToggleSet<number | string>();
  const mcp = useToggleSet<number | string>();
  const pipelines = useToggleSet<number | string>();
  const agents = useToggleSet<number | string>();
  const skills = useToggleSet<number | string>();

  const resetSelections = useCallback(() => {
    toolkits.reset();
    mcp.reset();
    pipelines.reset();
    agents.reset();
    skills.reset();
  }, [toolkits, mcp, pipelines, agents, skills]);

  const handleClose = useCallback(() => {
    // Invalidate any in-flight `handleGenerate` call so its continuation is a
    // no-op once it resolves — see the module doc comment's "abort-on-close,
    // one disclosed deviation" section.
    generationTokenRef.current += 1;
    setStep('input');
    setDescription('');
    setDraft(EMPTY_AGENT_DRAFT);
    resetGenerateError();
    resetSelections();
    onClose();
  }, [onClose, resetSelections, resetGenerateError]);

  const handleGenerate = useCallback(async () => {
    if (!description.trim() || projectId === undefined) return;

    const requestToken = ++generationTokenRef.current;
    setStep('loading');
    resetGenerateError();

    const response = await generateDraft({ projectId, user_description: description });
    if (requestToken !== generationTokenRef.current) {
      // The modal was closed while this request was in flight — discard the
      // stale result (success or error) instead of writing it into state.
      resetGenerateError();
      return;
    }

    if (response === undefined) {
      setStep('input');
      return;
    }

    setDraft(mapApplicationDraft(response));
    resetSelections();
    setStep('review');
  }, [description, projectId, generateDraft, resetGenerateError, resetSelections]);

  const handleDescriptionKeyDown = useCallback(
    (event: KeyboardEvent<HTMLDivElement>) => {
      if (event.key === 'Enter' && !event.shiftKey) {
        event.preventDefault();
        void handleGenerate();
      }
    },
    [handleGenerate],
  );

  // Baseline: `autoFocus` on the `STEPS.INPUT` `TextField`, focusing it as
  // soon as it appears. Imperative focus-on-attach via the `inputRef`
  // callback, not the JSX `autoFocus` prop (`jsx-a11y/no-autofocus`, R-C1,
  // bans it outright with no per-file waiver; same fix already applied
  // throughout this codebase, e.g. `FolderItemEditor.tsx`'s own doc comment)
  // and not a `useEffect` keyed on mount (this component never unmounts —
  // `GenerateAgentButton` always renders it, gated only by the `open` prop
  // passed to `BaseModal`'s `Dialog` — so a mount-only effect wouldn't
  // re-focus on every reopen or on "Back to prompt"; a ref-based
  // `open`/`step`-keyed effect was tried first, but MUI's `TextareaAutosize`
  // (the multiline `TextField`'s actual input) attaches its DOM ref during a
  // deferred internal render pass that lands AFTER this component's own
  // effects flush, leaving the ref `null` when such an effect would fire —
  // focusing directly in the `inputRef` callback, which necessarily runs at
  // the exact moment the node attaches, sidesteps that ordering hazard and
  // naturally re-fires on every fresh mount of this field).
  const focusDescriptionField = useCallback((element: HTMLTextAreaElement | HTMLInputElement | null) => {
    element?.focus();
  }, []);

  const handleBack = useCallback(() => {
    setStep('input');
    setDraft(EMPTY_AGENT_DRAFT);
    resetGenerateError();
  }, [resetGenerateError]);

  const handleApprove = useCallback(async () => {
    try {
      const result = await approve(draft, { selectedAgentIds: agents.selectedIds, selectedPipelineIds: pipelines.selectedIds });
      onAgentCreated(result);
      handleClose();
    } catch (error) {
      const message = generateErrorMessage(error);
      if (onApproveError) onApproveError(message);
      else console.error(message);
    }
  }, [approve, draft, agents.selectedIds, pipelines.selectedIds, onAgentCreated, handleClose, onApproveError]);

  const content =
    step === 'loading' ? (
      <Box sx={loadingContainerSx}>
        <CircularProgress size={24} />
        <Typography
          variant="bodyMedium"
          color="text.secondary"
        >
          {t('features.agents.generateAgentModal.generating', 'Generating agent draft...')}
        </Typography>
      </Box>
    ) : step === 'review' ? (
      <GenerateAgentReviewForm
        draft={draft}
        onChange={setDraft}
        onValidationChange={setIsDraftValid}
        selection={{
          toolkits: { selectedIds: toolkits.selectedIds, onToggle: toolkits.toggle },
          mcp: { selectedIds: mcp.selectedIds, onToggle: mcp.toggle },
          pipelines: { selectedIds: pipelines.selectedIds, onToggle: pipelines.toggle },
          agents: { selectedIds: agents.selectedIds, onToggle: agents.toggle },
          skills: { selectedIds: skills.selectedIds, onToggle: skills.toggle },
        }}
      />
    ) : (
      <Box sx={inputContainerSx}>
        <TextField
          fullWidth
          multiline
          minRows={10}
          maxRows={16}
          placeholder={t(
            'features.agents.generateAgentModal.placeholder',
            "Describe your agent's goal, key tasks, and preferred tone or behavior.",
          )}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          onKeyDown={handleDescriptionKeyDown}
          inputRef={focusDescriptionField}
          variant="standard"
          slotProps={{ input: { disableUnderline: true } }}
        />
        {generateError !== undefined && (
          <Alert
            severity="error"
            sx={errorAlertSx}
          >
            {generateErrorMessage(generateError)}
          </Alert>
        )}
      </Box>
    );

  const actions =
    step === 'loading' ? undefined : step === 'review' ? (
      <>
        <BaseBtn
          variant="secondary"
          onClick={handleBack}
          disabled={isApproving}
        >
          {t('features.agents.generateAgentModal.backToPrompt', 'Back to prompt')}
        </BaseBtn>
        <BaseBtn
          variant="contained"
          color="primary"
          onClick={() => void handleApprove()}
          disabled={isApproving || !isDraftValid}
        >
          {isApproving
            ? t('features.agents.generateAgentModal.creating', 'Creating...')
            : t('features.agents.generateAgentModal.createAgent', 'Create Agent')}
        </BaseBtn>
      </>
    ) : (
      <BaseBtn
        variant="contained"
        color="primary"
        disabled={!description.trim()}
        onClick={() => void handleGenerate()}
      >
        {t('features.agents.generateAgentModal.generate', 'Generate')}
      </BaseBtn>
    );

  return (
    <BaseModal
      open={open}
      title={t('features.agents.generateAgentModal.title', 'Build with AI')}
      onClose={handleClose}
      content={content}
      actions={{ node: actions }}
    />
  );
}

const loadingContainerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  justifyContent: 'center',
  gap: '0.75rem',
  padding: '2rem 0',
};
const inputContainerSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', minHeight: '16rem' };
const errorAlertSx: SxProps<Theme> = { marginTop: '0.5rem' };
