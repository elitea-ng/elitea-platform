/**
 * Semantic search in a workspace's index settings: off (the default: the
 * code's words and structure only), a model that runs on this computer
 * (the person picks one; it is downloaded only after they agree to its
 * size), or the deployment's embeddings model when the policy allows it
 * (the folder's code leaves the computer). Shows the embedding's progress
 * and why it waits (a turn, battery, Low Power Mode), with "Embed now".
 */
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import LinearProgress from '@mui/material/LinearProgress';
import Radio from '@mui/material/Radio';
import RadioGroup from '@mui/material/RadioGroup';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { EMBEDDINGS_OFF, type EmbedStatus, type ModelInfo } from '@/shared/desktop/indexIpc';
import { t } from '@/shared/i18n';

import { describeIndexError } from '../model/describeIndexError';
import { LOCAL_PREFIX, localModelOf, REMOTE_PREFIX, remoteModelOf, type WorkspaceEmbeddings } from '../model/useEmbeddings';

const mutedSx = (theme: Theme) => ({
  color: theme.vars.palette.text.secondary,
});

const REMOTE_OPTION = 'remote';

/** `470 MB`, `1.2 GB`. */
export function formatSize(bytes: number): string {
  if (bytes >= 1e9) return `${(bytes / 1e9).toFixed(1)} GB`;
  return `${Math.max(1, Math.round(bytes / 1e6))} MB`;
}

function modelLine(model: ModelInfo): string {
  const memory = t('workspace.index.embeddings.memory', 'about {{size}} of memory while embedding', { size: formatSize(model.ram_mb * 1e6) });
  if (model.state === 'ready') return t('workspace.index.embeddings.downloaded', 'Downloaded · {{memory}}', { memory });
  return t('workspace.index.embeddings.toDownload', '{{size}} download · {{memory}}', { size: formatSize(model.download_bytes), memory });
}

function statusLine(status: EmbedStatus): string {
  const counts = { done: String(status.done), total: String(status.total) };
  switch (status.state) {
    case 'running':
      return t('workspace.index.embeddings.running', 'Embedding: {{done}} of {{total}} chunks.', counts);
    case 'waiting':
      return t('workspace.index.embeddings.waiting', 'Waiting to embed ({{done}} of {{total}} chunks).', counts);
    case 'done':
      return t('workspace.index.embeddings.done', '{{done}} chunks embedded; searches use their meaning too.', counts);
    case 'error':
      return status.error ?? t('workspace.index.embeddings.failed', 'Embedding failed; it is tried again shortly.');
    case 'paused':
      switch (status.held) {
        case 'on_battery':
          return t('workspace.index.embeddings.onBattery', 'Paused on battery ({{done}} of {{total}}); it continues on power.', counts);
        case 'low_power':
          return t('workspace.index.embeddings.lowPower', 'Paused in Low Power Mode ({{done}} of {{total}}).', counts);
        case 'turn':
        case null:
          return t('workspace.index.embeddings.turn', 'Paused while an agent works ({{done}} of {{total}}).', counts);
      }
  }
}

function Consent({ model, embeddings }: { model: ModelInfo; embeddings: WorkspaceEmbeddings }): React.JSX.Element {
  const progress = embeddings.downloads[model.id];
  if (model.downloading || progress !== undefined) {
    const value = progress === undefined || progress.total === 0 ? undefined : (progress.done / progress.total) * 100;
    return (
      <Box data-testid="index-model-download" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
        <Typography variant="bodySmall">
          {progress === undefined
            ? t('workspace.index.embeddings.downloading', 'Downloading {{name}}…', { name: model.display_name })
            : t('workspace.index.embeddings.downloadingCount', 'Downloading {{name}}: {{done}} of {{total}}.', {
                name: model.display_name,
                done: formatSize(progress.done),
                total: formatSize(progress.total),
              })}
        </Typography>
        <LinearProgress variant={value === undefined ? 'indeterminate' : 'determinate'} value={value} />
        <Box>
          <Button size="small" disabled={embeddings.pending} onClick={() => embeddings.run({ kind: 'cancelDownload', modelId: model.id })}>
            {t('workspace.index.embeddings.cancelDownload', 'Cancel download')}
          </Button>
        </Box>
      </Box>
    );
  }
  return (
    <Alert
      severity="info"
      data-testid="index-model-consent"
      action={
        <Button size="small" disabled={embeddings.pending} onClick={() => embeddings.run({ kind: 'download', modelId: model.id })}>
          {model.state === 'partial' ? t('workspace.index.embeddings.resume', 'Resume download') : t('workspace.index.embeddings.download', 'Download')}
        </Button>
      }
    >
      {t(
        'workspace.index.embeddings.consent',
        'Download {{name}} ({{size}}) from Hugging Face? It is checked against a pinned checksum, kept in the app’s data and shared by every folder. Nothing from your code is sent.',
        { name: model.display_name, size: formatSize(model.download_bytes) },
      )}
    </Alert>
  );
}

function Remote({ embeddings }: { embeddings: WorkspaceEmbeddings }): React.JSX.Element {
  const [model, setModel] = useState(remoteModelOf(embeddings.choice) ?? '');
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1, paddingLeft: 4 }}>
      <Alert severity="warning">{t('workspace.index.embeddings.remoteWarning', 'This folder’s code is sent to your deployment’s embeddings model, paid from the bound project’s budget.')}</Alert>
      <Box sx={{ display: 'flex', gap: 1, alignItems: 'center' }}>
        <TextField size="small" label={t('workspace.index.embeddings.remoteModel', 'Embeddings model')} value={model} onChange={(event) => setModel(event.target.value)} />
        <Button
          variant="contained"
          disabled={embeddings.pending || model.trim() === ''}
          onClick={() =>
            embeddings.run({
              kind: 'choose',
              choice: `${REMOTE_PREFIX}${model.trim()}`,
            })
          }
        >
          {t('workspace.index.embeddings.useRemote', 'Use')}
        </Button>
      </Box>
    </Box>
  );
}

function Options({ embeddings, selected, onPick }: { embeddings: WorkspaceEmbeddings; selected: string; onPick: (value: string) => void }): React.JSX.Element | null {
  const { models } = embeddings;
  if (models === undefined) return null;
  const chosenLocal = localModelOf(embeddings.choice);
  return (
    <>
      <RadioGroup aria-label={t('workspace.index.embeddings.title', 'Semantic search')} value={selected} onChange={(event) => onPick(event.target.value)}>
        <FormControlLabel value={EMBEDDINGS_OFF} disabled={embeddings.pending} control={<Radio size="small" />} label={t('workspace.index.embeddings.off', 'Off')} />
        {models.models.map((model) => (
          <FormControlLabel
            key={model.id}
            value={`${LOCAL_PREFIX}${model.id}`}
            disabled={embeddings.pending || !models.local_allowed}
            control={<Radio size="small" />}
            label={
              <Box sx={{ display: 'flex', flexDirection: 'column' }}>
                <Typography variant="bodySmall">
                  {model.display_name}
                  {chosenLocal === model.id ? ` · ${t('workspace.index.embeddings.inUse', 'in use')}` : ''}
                </Typography>
                <Typography variant="bodySmall" sx={mutedSx}>
                  {model.summary} {modelLine(model)}
                </Typography>
              </Box>
            }
          />
        ))}
        {models.remote_allowed && (
          <FormControlLabel value={REMOTE_OPTION} disabled={embeddings.pending} control={<Radio size="small" />} label={t('workspace.index.embeddings.remote', 'Through your deployment')} />
        )}
      </RadioGroup>
      {!models.local_allowed && (
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.index.embeddings.localOff', 'Your organisation’s policy does not allow embedding models on this computer.')}
        </Typography>
      )}
    </>
  );
}

function Progress({ embeddings, status }: { embeddings: WorkspaceEmbeddings; status: EmbedStatus }): React.JSX.Element {
  return (
    <Box data-testid="index-embeddings-status" sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
      <Typography variant="bodySmall" sx={{ flex: 1 }}>
        {statusLine(status)}
      </Typography>
      {status.state === 'paused' && status.held !== 'turn' && (
        <Button size="small" disabled={embeddings.pending} onClick={() => embeddings.run({ kind: 'rebuild', full: false })}>
          {t('workspace.index.embeddings.now', 'Embed now')}
        </Button>
      )}
      {status.state === 'done' && (
        <Button size="small" disabled={embeddings.pending} onClick={() => embeddings.run({ kind: 'rebuild', full: true })}>
          {t('workspace.index.embeddings.reembed', 'Embed again')}
        </Button>
      )}
    </Box>
  );
}

/** The radio's value: a model awaiting its download, the deployment, or the stored choice. */
function selectedOption(embeddings: WorkspaceEmbeddings, remoteOpen: boolean): string {
  if (embeddings.awaiting !== null) return `${LOCAL_PREFIX}${embeddings.awaiting}`;
  if (remoteOpen || remoteModelOf(embeddings.choice) !== null) return REMOTE_OPTION;
  return embeddings.choice ?? EMBEDDINGS_OFF;
}

export function EmbeddingsSection({ embeddings }: { embeddings: WorkspaceEmbeddings }): React.JSX.Element {
  const { models, choice, status } = embeddings;
  const [remoteOpen, setRemoteOpen] = useState(false);
  const selected = selectedOption(embeddings, remoteOpen);
  const awaited = models?.models.find((model) => model.id === embeddings.awaiting);
  const pick = (value: string): void => {
    setRemoteOpen(value === REMOTE_OPTION);
    if (value !== REMOTE_OPTION) embeddings.run({ kind: 'choose', choice: value });
  };

  return (
    <Box component="section" data-testid="index-embeddings" sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      <Typography variant="labelMedium" component="h3" sx={{ margin: 0 }}>
        {t('workspace.index.embeddings.title', 'Semantic search')}
      </Typography>
      <Typography variant="bodySmall" sx={mutedSx}>
        {t('workspace.index.embeddings.body', 'Search always matches the code’s words and structure. To also match by meaning, choose a model: it runs on this computer and nothing is sent anywhere.')}
      </Typography>
      <Options embeddings={embeddings} selected={selected} onPick={pick} />
      {selected === REMOTE_OPTION && models?.remote_allowed === true && <Remote embeddings={embeddings} />}
      {awaited !== undefined && <Consent model={awaited} embeddings={embeddings} />}
      {embeddings.downloadError !== null && (
        <Alert severity="error">{t('workspace.index.embeddings.downloadFailed', 'The download failed: {{error}}', { error: embeddings.downloadError.error })}</Alert>
      )}
      {status !== null && choice !== EMBEDDINGS_OFF && <Progress embeddings={embeddings} status={status} />}
      {embeddings.actionError !== null && <Alert severity="error">{describeIndexError(embeddings.actionError)}</Alert>}
    </Box>
  );
}
