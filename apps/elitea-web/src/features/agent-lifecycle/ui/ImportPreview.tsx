/**
 * What Import shows before the write (issues 3137 and 3142): a header with
 * the team's (or the agent's) name and description, the agents as a roster
 * with their avatars (the zip's own media included), the lead marked and the
 * model each runs on, a labelled summary of what comes with them, and what
 * the person should know as one callout. A refusal stays here in the
 * server's words; an invalid file's refusal (422 `import_invalid`) also names
 * the field it refused.
 *
 * The Import team, the Import agent and the blueprint gallery's Import all
 * show this one preview.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { AgentRoster, AgentRosterRow, ROSTER_AVATAR_SIZE } from '@/shared/ui/AgentRoster';
import { Monogram } from '@/shared/ui/Monogram';

import type { ImportRefusal } from '../lib/importFollowUps';
import type { ImportDocumentSummary, ImportSummaryAgent } from '../lib/importSummary';
import { ImportNotices } from './ImportNotices';

/** Draws an agent's live avatar; the page passes the shared renderer (a feature does not import another feature). */
export type RenderImportAvatar = (avatar: { readonly iconUrl?: string | undefined; readonly pack?: object | undefined }, size: string) => ReactNode;

export interface ImportPreviewProps {
  readonly summary: ImportDocumentSummary;
  readonly refusal: ImportRefusal | undefined;
  /** The `blob:` URL of each agent's still from the zip, by version key. */
  readonly avatarUrls: ReadonlyMap<string, string>;
  readonly renderAvatar?: RenderImportAvatar | undefined;
  readonly testIdPrefix: string;
}

function ImportRefusalAlert({ refusal, testIdPrefix }: { readonly refusal: ImportRefusal; readonly testIdPrefix: string }): ReactNode {
  return (
    <Alert severity="error" data-testid={`${testIdPrefix}-import-refusal`}>
      {refusal.message}
      {refusal.field !== undefined && (
        <Typography variant="bodySmall" component="div" data-testid={`${testIdPrefix}-import-refusal-field`}>
          {t('features.agentLifecycle.import.refusedField', 'Field: {{field}}', { field: refusal.field })}
        </Typography>
      )}
    </Alert>
  );
}

/** The header's words: the team, else the one agent, else how many agents the file holds. */
function headerText(summary: ImportDocumentSummary): { readonly eyebrow: string; readonly title: string; readonly description: string } {
  if (summary.team !== undefined) {
    return { eyebrow: t('features.agentLifecycle.import.kindTeam', 'Team to import'), title: summary.team.name, description: summary.team.description };
  }
  const only = summary.agents.length === 1 ? summary.agents[0] : undefined;
  return {
    eyebrow: t('features.agentLifecycle.import.kindAgents', 'Agents to import'),
    title: only?.name ?? t('features.agentLifecycle.import.agentCount', '{{count}} agents', { count: summary.agents.length }),
    description: only?.description ?? '',
  };
}

interface PreviewHeaderProps {
  readonly summary: ImportDocumentSummary;
  /** A single agent's face and model line: its header is its whole preview. */
  readonly face?: ReactNode;
  readonly detail?: string | undefined;
  readonly testIdPrefix: string;
}

function PreviewHeader({ summary, face, detail, testIdPrefix }: PreviewHeaderProps): ReactNode {
  const { eyebrow, title, description } = headerText(summary);
  return (
    <Box sx={headerRowSx} data-testid={summary.team === undefined ? `${testIdPrefix}-import-header` : `${testIdPrefix}-import-team`}>
      {face !== undefined && (
        <Box aria-hidden sx={headerFaceSx}>
          {face}
        </Box>
      )}
      <Box sx={headerSx}>
        <Typography variant="labelSmall" sx={eyebrowSx}>
          {eyebrow}
        </Typography>
        <Typography component="h3" variant="headingSmall" sx={titleSx}>
          {title}
        </Typography>
        {description !== '' && (
          <Typography variant="bodyMedium" sx={descriptionSx}>
            {description}
          </Typography>
        )}
        {detail !== undefined && (
          <Typography variant="bodySmall" sx={descriptionSx} data-testid={`${testIdPrefix}-import-model`}>
            {detail}
          </Typography>
        )}
      </Box>
    </Box>
  );
}

function modelLine(agent: ImportSummaryAgent): string | undefined {
  if (agent.pipeline) return undefined;
  return agent.model === undefined
    ? t('features.agentLifecycle.import.modelDefault', 'Model: this project\'s default')
    : t('features.agentLifecycle.import.modelNamed', 'Model: {{model}}', { model: agent.model });
}

interface FaceProps {
  readonly agent: ImportSummaryAgent;
  readonly avatarUrls: ReadonlyMap<string, string>;
  readonly renderAvatar: RenderImportAvatar | undefined;
  readonly size?: string;
}

function Face({ agent, avatarUrls, renderAvatar, size = ROSTER_AVATAR_SIZE }: FaceProps): ReactNode {
  // The zip's own still, else an icon on this install; never a URL the file names elsewhere.
  const iconUrl = (agent.versionKey === undefined ? undefined : avatarUrls.get(agent.versionKey)) ?? agent.iconUrl;
  if (renderAvatar === undefined || iconUrl === undefined) {
    return <Monogram name={agent.name} size={size} variant="labelSmall" />;
  }
  return renderAvatar({ iconUrl }, size);
}

function Stat({ label, value, testId }: { readonly label: string; readonly value: number; readonly testId: string }): ReactNode {
  return (
    <Box sx={statSx} data-testid={testId}>
      <Typography component="dt" variant="labelSmall" sx={statLabelSx}>
        {label}
      </Typography>
      <Typography component="dd" variant="labelMedium" sx={statValueSx}>
        {value}
      </Typography>
    </Box>
  );
}

/** Agents, toolkits and skills always; datasets and memory seeds when the file carries some. */
function SummaryRow({ summary, testIdPrefix }: { readonly summary: ImportDocumentSummary; readonly testIdPrefix: string }): ReactNode {
  return (
    <Box component="dl" sx={statsSx} data-testid={`${testIdPrefix}-import-summary`}>
      <Stat label={t('features.agentLifecycle.import.statAgents', 'Agents')} value={summary.agents.length} testId={`${testIdPrefix}-import-stat-agents`} />
      <Stat label={t('features.agentLifecycle.import.toolkits', 'Toolkits')} value={summary.toolkitCount} testId={`${testIdPrefix}-import-stat-toolkits`} />
      <Stat label={t('features.agentLifecycle.import.skills', 'Skills')} value={summary.skillCount} testId={`${testIdPrefix}-import-stat-skills`} />
      {summary.datasetCount > 0 && (
        <Stat label={t('features.agentLifecycle.import.statDatasets', 'Eval datasets')} value={summary.datasetCount} testId={`${testIdPrefix}-import-stat-datasets`} />
      )}
      {summary.memorySeedCount > 0 && (
        <Stat label={t('features.agentLifecycle.import.statSeeds', 'Memory seeds')} value={summary.memorySeedCount} testId={`${testIdPrefix}-import-stat-seeds`} />
      )}
    </Box>
  );
}

function Roster({ summary, avatarUrls, renderAvatar, testIdPrefix }: Omit<ImportPreviewProps, 'refusal'>): ReactNode {
  const label =
    summary.team === undefined
      ? t('features.agentLifecycle.import.agentsList', 'Agents in this file')
      : t('features.agentLifecycle.import.membersList', 'Members of {{name}}', { name: summary.team.name });
  return (
    <AgentRoster label={label} data-testid={`${testIdPrefix}-import-members`}>
      {summary.agents.map((agent) => (
        <AgentRosterRow
          key={agent.key}
          face={<Face agent={agent} avatarUrls={avatarUrls} renderAvatar={renderAvatar} />}
          name={agent.name}
          detail={modelLine(agent)}
          lead={agent.lead}
          data-testid={`${testIdPrefix}-import-member`}
        />
      ))}
    </AgentRoster>
  );
}

export function ImportPreview({ summary, refusal, avatarUrls, renderAvatar, testIdPrefix }: ImportPreviewProps): ReactNode {
  const only = summary.team === undefined && summary.agents.length === 1 ? summary.agents[0] : undefined;
  return (
    <Box sx={rootSx}>
      {only === undefined ? (
        <>
          <PreviewHeader summary={summary} testIdPrefix={testIdPrefix} />
          <Roster summary={summary} avatarUrls={avatarUrls} renderAvatar={renderAvatar} testIdPrefix={testIdPrefix} />
        </>
      ) : (
        <PreviewHeader
          summary={summary}
          face={<Face agent={only} avatarUrls={avatarUrls} renderAvatar={renderAvatar} size={SINGLE_AVATAR_SIZE} />}
          detail={modelLine(only)}
          testIdPrefix={testIdPrefix}
        />
      )}
      <SummaryRow summary={summary} testIdPrefix={testIdPrefix} />
      <ImportNotices summary={summary} testIdPrefix={testIdPrefix} />
      {refusal !== undefined && <ImportRefusalAlert refusal={refusal} testIdPrefix={testIdPrefix} />}
    </Box>
  );
}

const SINGLE_AVATAR_SIZE = '3rem';

const rootSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(2), minWidth: 0 });
const headerRowSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'flex-start', gap: theme.spacing(1.5), minWidth: 0 });
const headerFaceSx: SxProps<Theme> = { display: 'flex', flexShrink: 0 };
const headerSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(0.5), flex: 1, minWidth: 0 });
const eyebrowSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary, textTransform: 'uppercase', letterSpacing: '0.04em' });
const titleSx: SxProps<Theme> = (theme: Theme) => ({ margin: 0, color: theme.vars.palette.text.primary, wordBreak: 'break-word' });
const descriptionSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary, wordBreak: 'break-word' });
const statsSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexWrap: 'wrap', gap: theme.spacing(1), margin: 0 });
const statSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(0.25),
  minWidth: theme.spacing(10),
  padding: theme.spacing(1, 1.5),
  borderRadius: theme.vars.shape.radiusMd,
  border: `1px solid ${theme.vars.palette.border.lines}`,
  background: theme.vars.palette.background.card.default,
});
const statLabelSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });
const statValueSx: SxProps<Theme> = (theme: Theme) => ({ margin: 0, color: theme.vars.palette.text.primary });
