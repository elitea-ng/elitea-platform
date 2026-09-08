import type { ReactNode } from 'react';
import { useCallback } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import type { PipelineInboundTrigger } from '@/shared/api/generated/model';
import { handleCopy } from '@/shared/lib/clipboard';

const rowSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const actionsSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.5rem' };
const urlSx: SxProps<Theme> = {
  wordBreak: 'break-all',
  padding: '0.5rem',
  borderRadius: (theme: Theme) => theme.vars.shape.radiusMd,
  backgroundColor: 'background.default',
  color: 'text.secondary',
};

/** What the card may offer, decided once so the JSX below reads as a layout. */
interface TriggerState {
  readonly configured: boolean;
  readonly revoked: boolean;
  readonly live: boolean;
  readonly showing: boolean;
  readonly disabled: boolean;
}

function triggerState(props: EditPipelineTriggerCardProps): TriggerState {
  const configured = props.trigger?.configured === true;
  const revoked = configured && typeof props.trigger?.revoked_at === 'string';
  return {
    configured,
    revoked,
    live: configured && !revoked,
    showing: props.secretUrl !== undefined,
    disabled: props.isBusy || props.isReadOnly,
  };
}

export interface EditPipelineTriggerCardProps {
  readonly trigger: PipelineInboundTrigger | undefined;
  readonly secretUrl: string | undefined;
  readonly isBusy: boolean;
  readonly isReadOnly: boolean;
  readonly onRotate: () => void;
  readonly onReveal: () => void;
  readonly onRevoke: () => void;
  readonly onHideSecret: () => void;
}

/** The buttons, split out to keep both this file and the card inside the §3.5 complexity budget. */
function TriggerActions(props: EditPipelineTriggerCardProps & { readonly state: TriggerState }): ReactNode {
  const { state, secretUrl, onRotate, onReveal, onRevoke, onHideSecret } = props;
  const onCopy = useCallback(() => {
    if (secretUrl !== undefined) void handleCopy(secretUrl);
  }, [secretUrl]);

  return (
    <Box sx={actionsSx}>
      <Button variant="contained" size="small" disabled={state.disabled} onClick={onRotate} data-testid="pipeline-trigger-rotate">
        {state.live
          ? t('pages.pipelines.editPipeline.triggers.trigger.rotate', 'Rotate')
          : t('pages.pipelines.editPipeline.triggers.trigger.create', 'Create trigger URL')}
      </Button>
      {state.live && !state.showing && (
        <Button variant="outlined" size="small" disabled={state.disabled} onClick={onReveal} data-testid="pipeline-trigger-reveal">
          {t('pages.pipelines.editPipeline.triggers.trigger.reveal', 'Show secret URL')}
        </Button>
      )}
      {state.showing && (
        <Button variant="outlined" size="small" onClick={onCopy} data-testid="pipeline-trigger-copy">
          {t('pages.pipelines.editPipeline.triggers.trigger.copy', 'Copy')}
        </Button>
      )}
      {state.showing && (
        <Button variant="text" size="small" onClick={onHideSecret} data-testid="pipeline-trigger-hide">
          {t('pages.pipelines.editPipeline.triggers.trigger.hide', 'Hide')}
        </Button>
      )}
      {state.live && (
        <Button variant="outlined" color="error" size="small" disabled={state.disabled} onClick={onRevoke} data-testid="pipeline-trigger-revoke">
          {t('pages.pipelines.editPipeline.triggers.trigger.revoke', 'Revoke')}
        </Button>
      )}
    </Box>
  );
}

/**
 * The inbound trigger — issue 192, in the pipeline's own settings.
 *
 * ## What is shown, and what is deliberately NOT
 *
 * The URL WITHOUT the credential is always visible. The credential appears
 * only after an explicit "Show secret URL" or a rotate, because the read that
 * populates this card on every open must not carry one: it would then sit in
 * the query cache and in any HAR a person exports, and a view-only member
 * would be able to copy a working webhook URL for a pipeline they cannot
 * edit. The two are separate operations on the backend for exactly that
 * reason, with the WRITE permission on the revealing one.
 *
 * ## A revoked trigger is shown as revoked, not as absent
 *
 * The row survives revocation on the server so that "was this live last
 * Tuesday" has an answer. Rendering it as "no trigger" would throw that away
 * at the last step, and would tell a person who has just revoked one that
 * nothing ever existed.
 */
export function EditPipelineTriggerCard(props: EditPipelineTriggerCardProps): ReactNode {
  const { trigger, secretUrl } = props;
  const state = triggerState(props);
  const lastUsed = trigger?.last_used_at;

  return (
    <Box sx={rowSx} data-testid="pipeline-trigger-card">
      <Typography variant="bodyMedium" color="text.secondary">
        {t(
          'pages.pipelines.editPipeline.triggers.trigger.description',
          'An external system can start this pipeline version by calling this URL with its secret.',
        )}
      </Typography>

      {!state.configured && (
        <Typography variant="bodyMedium" data-testid="pipeline-trigger-absent">
          {t('pages.pipelines.editPipeline.triggers.trigger.absent', 'This pipeline has no trigger URL.')}
        </Typography>
      )}

      {state.configured && (
        <Typography variant="bodySmall" sx={urlSx} data-testid="pipeline-trigger-url">
          {secretUrl ?? trigger?.url ?? ''}
        </Typography>
      )}

      {state.revoked && (
        <Typography variant="bodySmall" color="error" data-testid="pipeline-trigger-revoked">
          {t('pages.pipelines.editPipeline.triggers.trigger.revoked', 'This trigger is revoked and refuses every call.')}
        </Typography>
      )}

      {state.live && typeof lastUsed === 'string' && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="pipeline-trigger-last-used">
          {t('pages.pipelines.editPipeline.triggers.trigger.lastUsed', 'Last called: {{when}}', {
            when: new Date(lastUsed).toLocaleString(),
          })}
        </Typography>
      )}

      <TriggerActions {...props} state={state} />

      {state.live && (
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'pages.pipelines.editPipeline.triggers.trigger.headerHint',
            'Send the secret as an Authorization: Bearer header where you can. A URL is written to proxy logs.',
          )}
        </Typography>
      )}
    </Box>
  );
}
