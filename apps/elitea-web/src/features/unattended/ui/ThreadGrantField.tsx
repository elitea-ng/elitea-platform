/**
 * The grant of a schedule or a webhook trigger that starts turns on a
 * handed-off thread (ADR-0045 decision 9): the "Run to completion" control
 * without "Also answer for me". No person is there when an occurrence
 * starts, so Main refuses sensitive tools, hook asks and remote confirmations
 * from these origins; the control offers only the `continue` parts, the chain
 * limits and the goal check.
 *
 * It fills `entities/thread-target`'s grant slot (`app/App.tsx`). It is
 * controlled: it shows the form's draft and keeps no state of its own, so a
 * switch of "Runs" away and back, or a new stored target, shows what the
 * form will send. The control's own state of an edit travels in the draft
 * (`edit`), because chain limits that are not valid yet have no wire form.
 */
import type { ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import type { ThreadGrantDraft, ThreadGrantFieldProps } from '@/entities/thread-target';
import type { ChatUnattendedRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { isRecord } from '@/shared/lib/object';

import { fromCloudGrant, toCloudGrant, unattendedRefusalText } from '../model/cloud';
import { defaultChoice, type RunChoice } from '../model/runChoice';
import { RunToCompletionControl } from './RunToCompletionControl';

/** The control's state of a stored grant: off when the grant continues at no limit. */
function choiceOfGrant(grant: ChatUnattendedRequest): RunChoice {
  return { on: grant.continue_step_limit !== false || grant.continue_output_limit !== false, grant: fromCloudGrant(grant) };
}

/** The `edit` this field put in the draft, read back. */
function editOf(edit: unknown): RunChoice | undefined {
  return isRecord(edit) && typeof edit['on'] === 'boolean' && isRecord(edit['grant']) ? (edit as unknown as RunChoice) : undefined;
}

/** What the control shows for a draft: the person's edit, else the grant to send, else Main's default. */
function choiceOf(value: ThreadGrantDraft): RunChoice {
  if (value.kind === 'default') return defaultChoice();
  return editOf(value.edit) ?? (value.kind === 'set' ? choiceOfGrant(value.grant) : defaultChoice());
}

/** The draft of a choice: off continues at no limit; on continues at both, inside the typed chain limits. */
function draftOf(choice: RunChoice): ThreadGrantDraft {
  if (!choice.on) return { kind: 'set', grant: { continue_step_limit: false, continue_output_limit: false, goal_check: choice.grant.goal_check }, edit: choice };
  const wire = toCloudGrant({ ...choice.grant, continue_step_limit: true, continue_output_limit: true, sensitive_tools: [] });
  return wire.ok ? { kind: 'set', grant: wire.grant, edit: choice } : { kind: 'invalid', edit: choice };
}

export function ThreadGrantField({ value, onChange, refusal, disabled = false }: ThreadGrantFieldProps): ReactNode {
  const refused = refusal === undefined ? undefined : unattendedRefusalText(refusal);
  return (
    <>
      <span>
        <RunToCompletionControl
          value={choiceOf(value)}
          onChange={next => onChange(draftOf(next))}
          scope="cloud"
          optIns={false}
          disabled={disabled}
          testId="thread-target-grant"
        />
      </span>
      {value.kind === 'invalid' && (
        <Typography variant="bodySmall" color="error" role="alert" data-testid="thread-target-grant-invalid">
          {t('unattended.control.invalidSummary', 'A chain limit is not valid. Open the settings and correct it.')}
        </Typography>
      )}
      {refused !== undefined && (
        <Typography variant="bodySmall" color="error" role="alert" data-testid="thread-target-grant-error">
          {refused}
        </Typography>
      )}
    </>
  );
}
