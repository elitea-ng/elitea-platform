import type { ReactNode } from 'react';
import { useCallback, useEffect, useRef, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import { generateContentBlocking, readGeneratedContent, type AiEditLlmSettings } from '../../api/aiEdit';
import { buildAiEditInput, parseAiEditDraft, type AiEditFields } from '../../lib/aiEditDraft';
import { applicationErrorMessage } from '../../lib/errorMessage';
import { AiEditReviewForm } from './AiEditReviewForm';

/**
 * Port of `apps/elitea-ui/src/[fsd]/entities/edit-entity-with-ai/ui/
 * EditEntityModal.jsx` + its agent consumer, narrowed to the one field this
 * backend can honestly answer for.
 *
 * **SCOPE, and why it is the instructions field alone.** The baseline's
 * modal walks a multi-step wizard over `name`/`description`/`instructions`,
 * because its `generate_*_draft` endpoints returned a STRUCTURED draft
 * object. Those three routes were removed here (#126; `../../lib/
 * agentDraft.ts` traces the whole gap), and the endpoint that survives —
 * `predict_llm` — returns generated TEXT. `../../lib/agentDraft.ts` already
 * established this codebase's answer to that: the returned text becomes the
 * instructions, and no other field is invented from it. One field, one
 * diff, one decision, instead of a three-step wizard over two fields the
 * response cannot fill.
 *
 * **ABORT ON CLOSE** follows `GenerateAgentModal`'s token pattern: closing
 * bumps `generationTokenRef`, and the continuation checks it before writing
 * any state, so a response that lands after the modal closed is discarded
 * rather than reopening it into stale content. An `AbortController` is
 * additionally passed to the fetch, so unlike `GenerateAgentModal` the
 * request itself really is cancelled.
 *
 * **NO TOAST SYSTEM** (`features/notifications/lib/errorMessage.ts`'s
 * convention): failures render inline in the modal instead.
 */
export interface EditInstructionsWithAiModalProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly projectId: string;
  readonly current: AiEditFields;
  /** The resolved Service Prompt — the caller's gate guarantees it is non-empty. */
  readonly basePrompt: string;
  readonly llmSettings: AiEditLlmSettings;
  readonly onApply: (fields: AiEditFields) => void;
}

type Phase = 'prompt' | 'loading' | 'review';

export function EditInstructionsWithAiModal(props: EditInstructionsWithAiModalProps): ReactNode {
  const { open, onClose, projectId, current, basePrompt, llmSettings, onApply } = props;

  const [phase, setPhase] = useState<Phase>('prompt');
  const [request, setRequest] = useState('');
  const [proposed, setProposed] = useState<AiEditFields>(current);
  const [error, setError] = useState<string | undefined>(undefined);

  const generationTokenRef = useRef(0);
  const abortRef = useRef<AbortController | undefined>(undefined);

  useEffect(() => {
    if (open) return;
    setPhase('prompt');
    setRequest('');
    setProposed(current);
    setError(undefined);
  }, [open, current]);

  const handleClose = useCallback(() => {
    generationTokenRef.current += 1;
    abortRef.current?.abort();
    abortRef.current = undefined;
    onClose();
  }, [onClose]);

  const handleGenerate = useCallback(async (): Promise<void> => {
    if (request.trim() === '') return;
    const token = ++generationTokenRef.current;
    const controller = new AbortController();
    abortRef.current = controller;
    setPhase('loading');
    setError(undefined);
    try {
      const result = await generateContentBlocking(
        projectId,
        {
          user_input: buildAiEditInput(basePrompt, current, request.trim()),
          chat_history: [],
          llm_settings: llmSettings,
        },
        controller.signal,
      );
      if (token !== generationTokenRef.current) return;
      if (result.error !== undefined && result.error !== '') throw new Error(result.error);
      const content = readGeneratedContent(result);
      if (content.trim() === '') {
        setError(t('features.agents.aiEdit.emptyResult', 'The model returned nothing to apply. Try rephrasing.'));
        setPhase('prompt');
        return;
      }
      // The prompt asks for a JSON agent configuration; plain text stands for the instructions alone.
      setProposed(parseAiEditDraft(content, current) ?? { ...current, instructions: content });
      setPhase('review');
    } catch (caught) {
      if (token !== generationTokenRef.current) return;
      setError(
        applicationErrorMessage(caught) ||
          t('features.agents.aiEdit.generateFailed', 'Failed to generate. Please try again.'),
      );
      setPhase('prompt');
    } finally {
      abortRef.current = undefined;
    }
  }, [basePrompt, current, llmSettings, projectId, request]);

  const handleApply = useCallback(() => {
    onApply(proposed);
    handleClose();
  }, [handleClose, onApply, proposed]);

  const content =
    phase === 'loading' ? (
      <Box sx={loadingSx}>
        <CircularProgress size={24} />
        <Typography
          variant="bodySmall"
          color="text.secondary"
        >
          {t('features.agents.aiEdit.generating', 'Generating a draft…')}
        </Typography>
      </Box>
    ) : phase === 'review' ? (
      <AiEditReviewForm
        current={current}
        draft={proposed}
        onChange={setProposed}
      />
    ) : (
      <Box sx={promptSx}>
        <TextField
          fullWidth
          multiline
          minRows={8}
          maxRows={16}
          label={t('features.agents.aiEdit.requestLabel', 'What should change?')}
          placeholder={t('features.agents.aiEdit.requestPlaceholder', 'Describe the change you want to the instructions.')}
          value={request}
          onChange={(event) => setRequest(event.target.value)}
        />
        {error !== undefined && <Alert severity="error">{error}</Alert>}
      </Box>
    );

  const actionsNode =
    phase === 'review' ? (
      <>
        <BaseBtn
          variant="secondary"
          size="small"
          onClick={() => setPhase('prompt')}
        >
          {t('features.agents.aiEdit.refine', 'Refine prompt')}
        </BaseBtn>
        <BaseBtn
          variant="secondary"
          size="small"
          onClick={handleClose}
        >
          {t('common.cancel', 'Cancel')}
        </BaseBtn>
        <BaseBtn
          variant="elitea"
          size="small"
          disabled={proposed.instructions.trim() === ''}
          onClick={handleApply}
        >
          {t('features.agents.aiEdit.apply', 'Apply')}
        </BaseBtn>
      </>
    ) : phase === 'prompt' ? (
      <>
        <BaseBtn
          variant="secondary"
          size="small"
          onClick={handleClose}
        >
          {t('common.cancel', 'Cancel')}
        </BaseBtn>
        <BaseBtn
          variant="elitea"
          size="small"
          disabled={request.trim() === ''}
          onClick={() => void handleGenerate()}
        >
          {t('features.agents.aiEdit.generate', 'Generate draft')}
        </BaseBtn>
      </>
    ) : null;

  return (
    <BaseModal
      open={open}
      variant="complex"
      title={t('features.agents.aiEdit.title', 'Edit instructions with AI')}
      onClose={handleClose}
      content={content}
      data-testid="ai-edit-instructions-modal"
      {...(actionsNode === null ? {} : { actions: { node: actionsNode } })}
    />
  );
}

const loadingSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  justifyContent: 'center',
  gap: '0.75rem',
  minHeight: '12rem',
};
const promptSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem', minWidth: '30rem' };
