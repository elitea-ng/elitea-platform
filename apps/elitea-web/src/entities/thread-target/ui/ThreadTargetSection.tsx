/**
 * "Runs" — what a schedule or a webhook trigger starts:
 *
 *   (•) This pipeline / This agent        (the version runs)
 *   ( ) A handed-off workspace thread
 *       Thread  [ Fix the flaky test   v ]
 *       Message [ Run the checks and fix what fails. ]
 *       [∞ To completion v]                (the grant, without the opt-ins)
 *
 * The thread choice lists the caller's threads that are in the cloud now
 * (`useHandedOffThreads`). The grant field is a slot (`ThreadGrantFieldContext`);
 * where nothing fills it, the form sends no grant.
 */
import { useContext, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { RadioButtonGroup } from '@/shared/ui/RadioButtonGroup';
import { SingleSelect, type SingleSelectOption } from '@/shared/ui/SingleSelect';

import { useHandedOffThreads } from '../api/useHandedOffThreads';
import { THREAD_PROMPT_MAX, type ThreadTargetDraft, type ThreadTargetErrors } from '../model/threadTarget';
import { ThreadGrantFieldContext } from './threadGrantSlot';

/** What the version is: the first choice names it. */
export type VersionKind = 'pipeline' | 'agent';

/** The two choices: the version runs, or a turn starts on a thread. */
export type TargetChoice = 'version' | 'thread';

export interface ThreadTargetSectionProps {
  readonly projectId: string | undefined;
  readonly versionKind: VersionKind;
  readonly choice: TargetChoice;
  readonly onChoiceChange: (choice: TargetChoice) => void;
  readonly draft: ThreadTargetDraft;
  readonly onDraftChange: (draft: ThreadTargetDraft) => void;
  readonly errors?: ThreadTargetErrors | undefined;
  readonly disabled?: boolean | undefined;
}

const sectionSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem' };
const fieldsSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem', paddingLeft: '1.75rem' };

type FieldsProps = Omit<ThreadTargetSectionProps, 'versionKind' | 'choice' | 'onChoiceChange'>;

/** The thread picker: the threads in the cloud now, and a stored thread that is not (so the form still names it). */
function ThreadPicker({ projectId, draft, onDraftChange, errors, disabled = false }: FieldsProps): ReactNode {
  const { threads, names, isLoading, capped } = useHandedOffThreads(projectId);
  const options: SingleSelectOption[] = threads.map(thread => ({ label: thread.name, value: thread.uuid }));
  if (draft.conversationUuid !== '' && !threads.some(thread => thread.uuid === draft.conversationUuid)) {
    options.push({ label: names.get(draft.conversationUuid) ?? draft.conversationUuid, value: draft.conversationUuid });
  }
  const empty = !isLoading && threads.length === 0;
  const threadError = errors?.thread ?? (empty ? t('threadTarget.noThreads', 'You have no thread in the cloud. On the desktop app, send a thread to the cloud first.') : undefined);
  return (
    <>
      <SingleSelect
        id="thread-target-thread"
        label={t('threadTarget.thread', 'Thread')}
        placeholder={t('threadTarget.threadPlaceholder', 'Select a thread')}
        value={draft.conversationUuid}
        options={options}
        onChange={value => onDraftChange({ ...draft, conversationUuid: value })}
        disabled={disabled || isLoading}
        {...(threadError === undefined ? {} : { error: threadError })}
      />
      {capped && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="thread-target-capped">
          {t('threadTarget.capped', 'You have more than 1000 Local work threads. The list shows the first 1000.')}
        </Typography>
      )}
    </>
  );
}

function PromptField({ draft, onDraftChange, errors, disabled = false }: FieldsProps): ReactNode {
  return (
    <TextField
      size="small"
      multiline
      minRows={2}
      maxRows={8}
      label={t('threadTarget.prompt', 'Message')}
      value={draft.prompt}
      disabled={disabled}
      error={errors?.prompt !== undefined}
      helperText={errors?.prompt ?? t('threadTarget.promptHelp', 'Each run starts a turn on the thread with this message. {{length}} of {{max}} characters.', { length: String(draft.prompt.length), max: String(THREAD_PROMPT_MAX) })}
      onChange={event => onDraftChange({ ...draft, prompt: event.target.value })}
      slotProps={{ htmlInput: { maxLength: THREAD_PROMPT_MAX, 'data-testid': 'thread-target-prompt' } }}
    />
  );
}

function ThreadFields(props: FieldsProps): ReactNode {
  const { draft, onDraftChange, errors, disabled } = props;
  const GrantField = useContext(ThreadGrantFieldContext);
  return (
    <Box sx={fieldsSx} data-testid="thread-target-fields">
      <ThreadPicker {...props} />
      <PromptField {...props} />
      {GrantField !== null && (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.25rem' }}>
          <Typography variant="labelMedium">{t('threadTarget.grant', 'Run to completion')}</Typography>
          <GrantField
            value={draft.grant}
            onChange={grant => onDraftChange({ ...draft, grant })}
            refusal={errors?.grant}
            disabled={disabled}
          />
        </Box>
      )}
    </Box>
  );
}

export function ThreadTargetSection(props: ThreadTargetSectionProps): ReactNode {
  const { versionKind, choice, onChoiceChange, disabled = false, errors } = props;
  const versionLabel = versionKind === 'agent' ? t('threadTarget.choice.agent', 'This agent') : t('threadTarget.choice.pipeline', 'This pipeline');
  return (
    <Box sx={sectionSx} data-testid="thread-target-section">
      <Typography variant="labelMedium">{t('threadTarget.runs', 'Runs')}</Typography>
      <RadioButtonGroup
        aria-label={t('threadTarget.runs', 'Runs')}
        value={choice}
        disabled={disabled}
        items={[
          { value: 'version', label: versionLabel },
          { value: 'thread', label: t('threadTarget.choice.thread', 'A handed-off workspace thread') },
        ]}
        onChange={value => onChoiceChange(value === 'thread' ? 'thread' : 'version')}
      />
      {choice === 'thread' && <ThreadFields {...props} />}
      {errors?.form !== undefined && (
        <Typography variant="bodySmall" color="error" role="alert" data-testid="thread-target-error">
          {errors.form}
        </Typography>
      )}
    </Box>
  );
}
