/**
 * The grant of a schedule or a webhook trigger that starts turns on a
 * handed-off thread (ADR-0045 decision 9): the "Run to completion" control
 * without "Also answer for me". No person is there when an occurrence
 * starts, so Main refuses sensitive tools, hook asks and remote confirmations
 * from these origins; the control offers only the `continue` parts, the chain
 * limits and the goal check.
 *
 * It fills `entities/thread-target`'s grant slot (`app/providers`). It reports
 * a grant only after the person changes it: with no edit, the form sends the
 * stored grant, or none (Main's default).
 */
import { useState, type ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import type { ThreadGrantDraft, ThreadGrantFieldProps } from '@/entities/thread-target';
import type { ChatUnattendedRequest } from '@/shared/api/generated/model';

import { fromCloudGrant, toCloudGrant, unattendedRefusalText } from '../model/cloud';
import { defaultChoice, type RunChoice } from '../model/runChoice';
import { RunToCompletionControl } from './RunToCompletionControl';

/** The control's state of a stored grant: off when the grant continues at no limit. */
function choiceOf(stored: ChatUnattendedRequest | undefined): RunChoice {
  if (stored === undefined) return defaultChoice();
  return { on: stored.continue_step_limit !== false || stored.continue_output_limit !== false, grant: fromCloudGrant(stored) };
}

/** The grant a choice sends: off continues at no limit; on continues at both, inside the typed chain limits. */
function draftOf(choice: RunChoice): ThreadGrantDraft {
  if (!choice.on) return { kind: 'set', grant: { continue_step_limit: false, continue_output_limit: false, goal_check: choice.grant.goal_check } };
  const wire = toCloudGrant({ ...choice.grant, continue_step_limit: true, continue_output_limit: true, sensitive_tools: [] });
  return wire.ok ? { kind: 'set', grant: wire.grant } : { kind: 'invalid' };
}

export function ThreadGrantField({ stored, onChange, refusal, disabled = false }: ThreadGrantFieldProps): ReactNode {
  const [choice, setChoice] = useState<RunChoice>(() => choiceOf(stored));
  const refused = refusal === undefined ? undefined : unattendedRefusalText(refusal);
  return (
    <>
      <span>
        <RunToCompletionControl
          value={choice}
          onChange={next => {
            setChoice(next);
            onChange(draftOf(next));
          }}
          scope="cloud"
          optIns={false}
          disabled={disabled}
          testId="thread-target-grant"
        />
      </span>
      {refused !== undefined && (
        <Typography variant="bodySmall" color="error" role="alert" data-testid="thread-target-grant-error">
          {refused}
        </Typography>
      )}
    </>
  );
}
