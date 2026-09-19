import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import RefreshIcon from '@mui/icons-material/Refresh';
import VisibilityIcon from '@mui/icons-material/Visibility';
import VisibilityOffIcon from '@mui/icons-material/VisibilityOff';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { SxProps, Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';

/**
 * The inbound trigger's settings dialog (baseline:
 * `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/settings/
 * PipelineWebhookModal.jsx`, unit A2h), rebuilt on the Go inbound route by
 * #899.
 *
 * WHAT WENT, AND WHY. The baseline offered GitHub / GitLab / Custom webhook
 * "types", each naming a different signature header
 * (`x-hub-signature-256`, `x-gitlab-token`, `X-Webhook-Token`), because
 * pylon's endpoint verified all three. `POST /pipeline_trigger/{project}/
 * {token}` verifies ONE credential, presented as `Authorization: Bearer`, as
 * `X-Elitea-Trigger-Token`, or as a `token` query parameter, compared in
 * constant time against a stored SHA-256 (`internal/api/v2/pipelinetriggers/
 * inbound.go`). There is no per-type signature mode to choose, so the radio
 * group is gone rather than left selecting something the backend ignores.
 *
 * The secret is likewise no longer generated in the browser. The backend
 * mints it, returns it exactly once on create/rotate, stores only its digest
 * beside the row and its plaintext in the vault's hidden bucket, and hands it
 * back only through the separate reveal operation that carries the WRITE
 * permission. So this dialog has a Reveal action instead of a value it holds:
 * with nothing revealed it shows the URL alone, which is what a view of a
 * trigger looks like when the credential has not been asked for.
 */

export interface PipelineWebhookModalProps {
  readonly open: boolean;
  readonly onClose: () => void;
  /** The inbound URL WITHOUT the secret — present on every read. */
  readonly webhookUrl?: string | undefined;
  /** The live credential, present only after a create/rotate or a reveal. */
  readonly secretValue?: string | undefined;
  readonly isLoading?: boolean | undefined;
  readonly onReveal: () => void;
  readonly onRotate: () => void;
  readonly onRevoke: () => void;
  /** Fires with a short confirmation message on copy actions (baseline: `toastSuccess`). See `TriggerTypeSelector.tsx`'s doc comment for the "no global toast hook" convention this replaces. */
  readonly onNotify?: ((message: string) => void) | undefined;
}

const contentWrapperSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.5rem', minWidth: '25rem' };
const sectionSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const descriptionSx: SxProps<Theme> = { color: 'text.secondary' };
const rowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem' };
const actionRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem', flexWrap: 'wrap' };
function valueInputSx(theme: Theme) {
  return { flex: 1, '& input': { fontSize: theme.typography.bodySmall.fontSize, fontFamily: 'monospace' } };
}
function codeBlockSx(theme: Theme) {
  return { backgroundColor: theme.vars.palette.background.secondary, border: `1px solid ${theme.vars.palette.border.lines}`, borderRadius: theme.vars.shape.radiusMd, padding: '0.75rem', overflow: 'auto', maxHeight: '12rem' };
}
function codeTextSx(theme: Theme) {
  return { fontFamily: 'monospace', fontSize: theme.typography.bodySmall2.fontSize, color: theme.vars.palette.text.secondary, whiteSpace: 'pre-wrap' as const, wordBreak: 'break-all' as const, margin: 0 };
}

/** The header form is what the backend's own documentation shows first: a URL is written to proxy logs, and a credential in one outlives the request. */
function buildExampleRequest(url: string, secret: string | undefined, showSecret: boolean): string | null {
  if (!url) return null;
  const displaySecret = showSecret && secret !== undefined ? secret : '<your_secret>';
  return `curl -X POST "${url}" \\\n  -H "Content-Type: application/json" \\\n  -H "Authorization: Bearer ${displaySecret}" \\\n  -d '{"input": "Your message or data here"}'`;
}

interface ValueRowProps {
  readonly label: string;
  readonly value: string;
  readonly testId: string;
  readonly copyTooltip: string;
  readonly onCopy: () => void;
  readonly extra?: ReactNode;
}

function ValueRow({ label, value, testId, copyTooltip, onCopy, extra }: ValueRowProps): ReactNode {
  return (
    <Box sx={sectionSx}>
      <Typography variant="labelMedium">{label}</Typography>
      <Box sx={rowSx}>
        <InputBase
          value={value}
          slotProps={{ htmlInput: { readOnly: true, 'data-testid': testId } }}
          sx={valueInputSx}
        />
        {extra}
        <Tooltip
          title={copyTooltip}
          placement="top"
        >
          <IconButton
            onClick={onCopy}
            aria-label={copyTooltip}
          >
            <ContentCopyIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      </Box>
    </Box>
  );
}

export function PipelineWebhookModal(props: PipelineWebhookModalProps): ReactNode {
  const { open, onClose, webhookUrl, secretValue, isLoading = false, onReveal, onRotate, onRevoke, onNotify } = props;

  const [showSecret, setShowSecret] = useState(false);

  useEffect(() => {
    if (open) setShowSecret(false);
  }, [open]);

  const copyToClipboard = useCallback(
    (value: string | undefined, message: string) => {
      if (!value) return;
      void navigator.clipboard.writeText(value);
      onNotify?.(message);
    },
    [onNotify],
  );

  const fullWebhookUrl = useMemo(() => (webhookUrl ? new URL(webhookUrl, window.location.origin).toString() : ''), [webhookUrl]);
  const exampleRequest = useMemo(() => buildExampleRequest(fullWebhookUrl, secretValue, showSecret), [fullWebhookUrl, secretValue, showSecret]);

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={t('pipelines.pipelineWebhookModal.title', 'Webhook settings')}
      content={
        <Box sx={contentWrapperSx}>
          {fullWebhookUrl && (
            <ValueRow
              label={t('pipelines.pipelineWebhookModal.webhookUrl', 'Webhook URL')}
              value={fullWebhookUrl}
              testId="pipeline-webhook-url"
              copyTooltip={t('pipelines.pipelineWebhookModal.copyUrl', 'Copy URL')}
              onCopy={() => copyToClipboard(fullWebhookUrl, t('pipelines.pipelineWebhookModal.urlCopied', 'Webhook URL copied to clipboard'))}
            />
          )}

          {secretValue === undefined ? (
            <Box sx={sectionSx}>
              <Typography variant="labelMedium">{t('pipelines.pipelineWebhookModal.secretValue', 'Secret Value')}</Typography>
              <Typography
                variant="bodySmall"
                sx={descriptionSx}
              >
                {t('pipelines.pipelineWebhookModal.secretHidden', 'The credential is stored server-side and is never part of the ordinary read. Reveal it to copy it again, or rotate it to replace it.')}
              </Typography>
            </Box>
          ) : (
            <ValueRow
              label={t('pipelines.pipelineWebhookModal.secretValue', 'Secret Value')}
              value={showSecret ? secretValue : '•'.repeat(Math.min(secretValue.length, 32))}
              testId="pipeline-webhook-secret"
              copyTooltip={t('pipelines.pipelineWebhookModal.copySecret', 'Copy secret')}
              onCopy={() => copyToClipboard(secretValue, t('pipelines.pipelineWebhookModal.secretCopied', 'Secret copied to clipboard'))}
              extra={
                <Tooltip
                  title={showSecret ? t('pipelines.pipelineWebhookModal.hideSecret', 'Hide secret') : t('pipelines.pipelineWebhookModal.showSecret', 'Show secret')}
                  placement="top"
                >
                  <IconButton
                    onClick={() => setShowSecret(previous => !previous)}
                    aria-label={showSecret ? t('pipelines.pipelineWebhookModal.hideSecret', 'Hide secret') : t('pipelines.pipelineWebhookModal.showSecret', 'Show secret')}
                  >
                    {showSecret ? <VisibilityOffIcon fontSize="small" /> : <VisibilityIcon fontSize="small" />}
                  </IconButton>
                </Tooltip>
              }
            />
          )}

          <Box sx={actionRowSx}>
            <BaseBtn
              variant="secondary"
              onClick={onReveal}
              disabled={isLoading}
              data-testid="pipeline-webhook-reveal"
              startIcon={<VisibilityIcon fontSize="small" />}
            >
              {t('pipelines.pipelineWebhookModal.reveal', 'Reveal secret')}
            </BaseBtn>
            <BaseBtn
              variant="secondary"
              onClick={onRotate}
              disabled={isLoading}
              data-testid="pipeline-webhook-rotate"
              startIcon={<RefreshIcon fontSize="small" />}
            >
              {t('pipelines.pipelineWebhookModal.rotate', 'Rotate secret')}
            </BaseBtn>
            <BaseBtn
              variant="secondary"
              color="error"
              onClick={onRevoke}
              disabled={isLoading}
              data-testid="pipeline-webhook-revoke"
              startIcon={<DeleteOutlineIcon fontSize="small" />}
            >
              {t('pipelines.pipelineWebhookModal.revoke', 'Revoke')}
            </BaseBtn>
          </Box>

          <Box sx={sectionSx}>
            <Typography variant="labelMedium">{t('pipelines.pipelineWebhookModal.payloadFormat', 'Payload Format')}</Typography>
            <Typography
              variant="bodySmall"
              sx={descriptionSx}
            >
              {t('pipelines.pipelineWebhookModal.payloadDescription', 'Send a POST request with an optional JSON body. Its `input` field is placed in front of the pipeline; an unparseable body is accepted as empty input.')}
            </Typography>
          </Box>

          {exampleRequest !== null && (
            <Box sx={sectionSx}>
              <Box sx={rowSx}>
                <Typography
                  variant="labelMedium"
                  sx={{ flex: 1 }}
                >
                  {t('pipelines.pipelineWebhookModal.exampleRequest', 'Example Request')}
                </Typography>
                <Tooltip
                  title={t('pipelines.pipelineWebhookModal.copyExample', 'Copy example')}
                  placement="top"
                >
                  <IconButton
                    onClick={() => copyToClipboard(exampleRequest, t('pipelines.pipelineWebhookModal.exampleCopied', 'Example request copied to clipboard'))}
                    aria-label={t('pipelines.pipelineWebhookModal.copyExample', 'Copy example')}
                  >
                    <ContentCopyIcon fontSize="small" />
                  </IconButton>
                </Tooltip>
              </Box>
              <Box sx={codeBlockSx}>
                <Typography
                  component="pre"
                  sx={codeTextSx}
                >
                  {exampleRequest}
                </Typography>
              </Box>
            </Box>
          )}
        </Box>
      }
      actions={{
        confirming: isLoading,
        confirmText: t('pipelines.pipelineWebhookModal.done', 'Done'),
        cancelText: t('pipelines.pipelineWebhookModal.cancel', 'Cancel'),
      }}
      onConfirm={onClose}
    />
  );
}
