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
import { SingleSelect } from '@/shared/ui/SingleSelect';

import {
  DEFAULT_SIGNATURE_HEADER,
  MODE_OPTIONS,
  WEBHOOK_MODES,
  buildExampleRequest,
  webhookModeFromAuthMode,
  type WebhookMode,
} from './pipelineWebhookModal.lib';

/**
 * The inbound trigger's settings dialog (baseline:
 * `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/settings/
 * PipelineWebhookModal.jsx`, unit A2h), rebuilt on the Go inbound route by
 * #899.
 *
 * THE TYPE SELECTOR IS BACK, WITH TWO ENTRIES AND NOT THREE (#970). The
 * baseline offered GitHub / GitLab / Custom, each naming a different
 * signature header, because pylon's endpoint verified all three. When this
 * dialog was rebuilt the Go route verified ONE credential — a bearer secret
 * in one of three carriers — so the group was removed rather than left
 * selecting something the backend ignored. The route now carries a per-trigger
 * MODE, so the choice exists again and means something:
 *
 *   - Custom keeps the bearer secret and the three carriers;
 *   - GitHub verifies `HMAC-SHA256(secret, raw body)` out of
 *     `X-Hub-Signature-256`, which is what a repository webhook sends — it
 *     sends no `Authorization` header and cannot be made to.
 *
 * GitLab is NOT offered: `X-Gitlab-Token` is a shared token sent verbatim
 * rather than a signature, so it is the Custom mode with a different header
 * name, and listing it would imply a verification this service does not
 * perform.
 *
 * CHANGING THE MODE ROTATES THE CREDENTIAL, because the backend writes the
 * mode on the create/rotate route and nowhere else. That is stated in the
 * dialog rather than done quietly: the previous secret stops working the
 * moment the mode changes, and whoever configured the sender has to paste the
 * new one.
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
  /** The stored `auth_mode` — `hmac_sha256` selects the GitHub entry. */
  readonly authMode?: string | undefined;
  /** The header the sender must sign into, for a signing trigger. */
  readonly signatureHeader?: string | undefined;
  readonly isLoading?: boolean | undefined;
  readonly onReveal: () => void;
  /** Rotates the credential. The mode is passed because the rotate route is where the backend writes it. */
  readonly onRotate: (mode?: WebhookMode) => void;
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

interface WebhookModeSectionProps {
  readonly selectedMode: WebhookMode;
  readonly storedMode: WebhookMode;
  readonly signatureHeader: string | undefined;
  readonly isLoading: boolean;
  readonly onSelect: (mode: WebhookMode) => void;
  readonly onApply: () => void;
}

/**
 * The type selector, its one-sentence explanation, and the "applying this
 * rotates the credential" confirmation.
 *
 * Its own component because the dialog function is at the §3.5 complexity
 * budget (12) and this section carries three branches of its own — not because
 * anything here is reusable.
 */
function WebhookModeSection({ selectedMode, storedMode, signatureHeader, isLoading, onSelect, onApply }: WebhookModeSectionProps): ReactNode {
  const hint =
    selectedMode === WEBHOOK_MODES.github
      ? t(
          'pipelines.pipelineWebhookModal.modeGithubHint',
          'GitHub signs the request body with the secret and sends the digest in {{header}}. Configure that secret in the repository\u2019s webhook settings; no Authorization header is sent or accepted.',
          { header: signatureHeader ?? DEFAULT_SIGNATURE_HEADER },
        )
      : t(
          'pipelines.pipelineWebhookModal.modeCustomHint',
          'The sender presents the secret itself, as an Authorization: Bearer header, as X-Elitea-Trigger-Token, or as a token query parameter.',
        );
  return (
    <Box sx={sectionSx}>
      <Typography variant="labelMedium">{t('pipelines.pipelineWebhookModal.modeLabel', 'Webhook type')}</Typography>
      <SingleSelect
        value={selectedMode}
        options={MODE_OPTIONS}
        onChange={value => onSelect(value === WEBHOOK_MODES.github ? WEBHOOK_MODES.github : WEBHOOK_MODES.custom)}
        disabled={isLoading}
        id="pipeline-webhook-mode"
      />
      <Typography
        variant="bodySmall"
        sx={descriptionSx}
        data-testid="pipeline-webhook-mode-hint"
      >
        {hint}
      </Typography>
      {selectedMode !== storedMode && (
        <Box sx={rowSx}>
          <Typography
            variant="bodySmall"
            sx={descriptionSx}
            data-testid="pipeline-webhook-mode-pending"
          >
            {t(
              'pipelines.pipelineWebhookModal.modePending',
              'Applying this type rotates the credential: the current secret stops working and the sender has to be given the new one.',
            )}
          </Typography>
          <BaseBtn
            variant="secondary"
            onClick={onApply}
            disabled={isLoading}
            data-testid="pipeline-webhook-apply-mode"
          >
            {t('pipelines.pipelineWebhookModal.applyMode', 'Apply and rotate')}
          </BaseBtn>
        </Box>
      )}
    </Box>
  );
}

export function PipelineWebhookModal(props: PipelineWebhookModalProps): ReactNode {
  const { open, onClose, webhookUrl, secretValue, authMode, signatureHeader, isLoading = false, onReveal, onRotate, onRevoke, onNotify } = props;

  const [showSecret, setShowSecret] = useState(false);
  const storedMode: WebhookMode = webhookModeFromAuthMode(authMode);
  // The SELECTED mode is local until the rotate that writes it: the backend
  // has no way to change a trigger's mode without minting a new credential, so
  // the dialog must be able to show the pending choice beside the warning that
  // applying it replaces the secret.
  const [selectedMode, setSelectedMode] = useState<WebhookMode>(storedMode);

  useEffect(() => {
    if (open) {
      setShowSecret(false);
      setSelectedMode(storedMode);
    }
    // `storedMode` is derived from a prop that can change while the dialog is
    // open (a rotate answers with the new mode); re-seeding the selection on
    // every open is what makes "what is stored" the starting point each time.
  }, [open, storedMode]);

  const copyToClipboard = useCallback(
    (value: string | undefined, message: string) => {
      if (!value) return;
      void navigator.clipboard.writeText(value);
      onNotify?.(message);
    },
    [onNotify],
  );

  const fullWebhookUrl = useMemo(() => (webhookUrl ? new URL(webhookUrl, window.location.origin).toString() : ''), [webhookUrl]);
  const storedSignatureHeader = storedMode === WEBHOOK_MODES.github ? (signatureHeader ?? DEFAULT_SIGNATURE_HEADER) : undefined;
  const exampleRequest = useMemo(
    () => buildExampleRequest({ url: fullWebhookUrl, secret: secretValue, showSecret, signatureHeader: storedSignatureHeader }),
    [fullWebhookUrl, secretValue, showSecret, storedSignatureHeader],
  );

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={t('pipelines.pipelineWebhookModal.title', 'Webhook settings')}
      content={
        <Box sx={contentWrapperSx}>
          <WebhookModeSection
            selectedMode={selectedMode}
            storedMode={storedMode}
            signatureHeader={signatureHeader}
            isLoading={isLoading}
            onSelect={setSelectedMode}
            onApply={() => onRotate(selectedMode)}
          />

          {storedSignatureHeader !== undefined && (
            <ValueRow
              label={t('pipelines.pipelineWebhookModal.signatureHeader', 'Signature header')}
              value={storedSignatureHeader}
              testId="pipeline-webhook-signature-header"
              copyTooltip={t('pipelines.pipelineWebhookModal.copyHeader', 'Copy header name')}
              onCopy={() => copyToClipboard(storedSignatureHeader, t('pipelines.pipelineWebhookModal.headerCopied', 'Signature header copied to clipboard'))}
            />
          )}

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
              onClick={() => onRotate(storedMode)}
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
