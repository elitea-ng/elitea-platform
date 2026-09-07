/**
 * Start, watch and stop a wiki generation (DWIKI-005), and pick it back up
 * after a reload (DWIKI-006).
 *
 * THE REDUCER IS `features/wiki-generation`; this panel owns only the edges:
 * the slots check before starting, the request that starts a run, the DELETE
 * that stops one, and the storage that remembers a running invocation so a
 * reload polls it again rather than showing an idle screen over a run that is
 * still going.
 *
 * A GENERATION NEEDS ONE SOURCE, and there are two kinds. A repository
 * toolkit arrives as `configuration.parameters.code_toolkit`, an integer
 * naming a configuration. An artifact folder arrives as
 * `artifact_configuration` inside the settings themselves and needs no
 * reference at all. A toolkit naming NEITHER is refused HERE rather than
 * sent — the facade's 400 would arrive as "the generation failed" with no
 * field to fix — and a toolkit naming both is refused by the facade, which is
 * why only one of them is ever put in the body.
 */
import { useCallback, useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Chip from '@mui/material/Chip';
import FormControlLabel from '@mui/material/FormControlLabel';
import Stack from '@mui/material/Stack';
import Switch from '@mui/material/Switch';
import ToggleButton from '@mui/material/ToggleButton';
import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';
import Typography from '@mui/material/Typography';
import { useQueryClient } from '@tanstack/react-query';

import { getArtifactSource, type ToolkitSettings } from '@/entities/wiki';
import { invocationIdFrom } from '@/entities/provider-run';
import { useWikiGeneration, type GenerationState } from '@/features/wiki-generation';
import {
  cancelDeepWikiInvocation,
  getDeepWikiInvocation,
  invokeDeepWikiTool,
  useGetDeepWikiSlots,
} from '@/shared/api/generated/deepwiki/deepwiki';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import { createGenerationStorage } from '../lib/generationStorage';

/** The provider's own toolkit name, which the SPI path carries. */
const WIKI_TOOLKIT_NAME = 'wikis';
const TOOL = 'generate_wiki';
/** The legacy cadence for the slots poll. */
const SLOTS_REFETCH_MS = 5000;

type PlannerMode = 'deepagents' | 'cluster';

interface WikiGenerationPanelProps {
  readonly projectId: string | number;
  readonly toolkitId: string | number;
  readonly settings: ToolkitSettings;
  /** Whether a wiki already exists, which turns Generate into a confirmed regenerate. */
  readonly hasWiki: boolean;
}

interface SlotsBody {
  readonly can_start?: boolean;
  readonly total?: number;
  readonly active?: number;
}

function slotsLabel(slots: SlotsBody | undefined): string {
  if (slots === undefined) return t('deepwiki.slots.checking', 'Checking slots…');
  return t('deepwiki.slots.status', '{{active}} of {{total}} slots in use', {
    active: String(slots.active ?? 0),
    total: String(slots.total ?? 0),
  });
}

function generateLabel(running: boolean, canStart: boolean): string {
  if (running) return t('deepwiki.generate.running', 'Generating…');
  if (!canStart) return t('deepwiki.generate.slotsBusy', 'All slots busy');
  return t('deepwiki.generate.start', 'Generate wiki');
}

function codeToolkitOf(settings: ToolkitSettings): number | null {
  const raw = settings['code_toolkit'] ?? settings['toolkit_configuration_code_toolkit'];
  const id = typeof raw === 'number' ? raw : typeof raw === 'string' ? Number(raw) : NaN;
  return Number.isInteger(id) && id > 0 ? id : null;
}

/**
 * The `configuration.parameters` a generation is started with.
 *
 * ONE SOURCE, NEVER TWO. A folder source travels in the settings themselves
 * (`artifact_configuration`), and the facade refuses a body naming a
 * `code_toolkit` beside one with a 400 — so the reference is dropped here
 * rather than spread in, which is what a toolkit that used to name a
 * repository still carries. Null means the settings name no source at all;
 * only then does this panel refuse, naming the setting.
 */
function generationParameters(settings: ToolkitSettings): Record<string, unknown> | null {
  if (getArtifactSource(settings) === null) {
    const codeToolkit = codeToolkitOf(settings);
    return codeToolkit === null ? null : { ...settings, code_toolkit: codeToolkit };
  }
  const parameters: Record<string, unknown> = { ...settings };
  delete parameters['code_toolkit'];
  delete parameters['toolkit_configuration_code_toolkit'];
  return parameters;
}

export function WikiGenerationPanel({ projectId, toolkitId, settings, hasWiki }: WikiGenerationPanelProps): React.JSX.Element {
  const queryClient = useQueryClient();
  const storage = useMemo(() => createGenerationStorage(projectId, toolkitId), [projectId, toolkitId]);
  // A reload RESUMES: the stored invocation is polled again from the first
  // render rather than the screen showing idle over a run still going.
  const [invocationId, setInvocationId] = useState<string | null>(() => storage.load()?.invocationId ?? null);
  const [planner, setPlanner] = useState<PlannerMode>('cluster');
  const [excludeTests, setExcludeTests] = useState(true);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [startError, setStartError] = useState<string | null>(null);
  const [stopping, setStopping] = useState(false);

  const slots = useGetDeepWikiSlots(Number(projectId), { query: { refetchInterval: SLOTS_REFETCH_MS, retry: false } });
  const slotsBody = slots.data === undefined ? undefined : (unwrapBody(slots.data) as SlotsBody | undefined);
  const canStart = slotsBody?.can_start !== false;

  const onSettled = useCallback(
    (state: GenerationState) => {
      // Forget the run: a reload after this must not poll a finished
      // invocation and show it as running (DWIKI-006's second clause).
      storage.clear();
      if (state.status.status === 'completed') {
        void queryClient.invalidateQueries({ queryKey: ['deepwiki'] });
      }
    },
    [queryClient, storage],
  );

  const generation = useWikiGeneration(invocationId, {
    poll: async (id) => {
      const body = unwrapBody(await getDeepWikiInvocation(Number(projectId), WIKI_TOOLKIT_NAME, TOOL, id));
      return body as never;
    },
    onSettled,
  });
  // RUNNING is "there is an invocation and it has not settled", not "the
  // reducer has said running". A resumed run's first poll can carry no events
  // at all, which leaves the reducer at `idle` while the provider is still
  // working — and a Stop button that hides behind that would leave the user no
  // way to stop the run they just reloaded into.
  const settled = generation.status.status === 'completed' || generation.status.status === 'error';
  const running = invocationId !== null && !settled;

  // Stopping is a DELETE on the invocation — the facade's cancel, not a task
  // API; the poll that follows reports Stopped, which the reducer reads as an
  // error status with the provider's own message.
  const stop = useCallback(async () => {
    if (invocationId === null) return;
    setStopping(true);
    try {
      await cancelDeepWikiInvocation(Number(projectId), WIKI_TOOLKIT_NAME, TOOL, invocationId);
    } catch (cause) {
      setStartError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setStopping(false);
    }
  }, [invocationId, projectId]);

  const start = useCallback(async () => {
    setConfirmOpen(false);
    setStartError(null);
    const parameters = generationParameters(settings);
    if (parameters === null) {
      setStartError(t('deepwiki.generate.noSource', 'The toolkit names no source. Set code_toolkit for a repository toolkit to clone, or choose an artifact folder to read.'));
      return;
    }
    try {
      const response = await invokeDeepWikiTool(Number(projectId), WIKI_TOOLKIT_NAME, TOOL, {
        configuration: { parameters },
        parameters: {
          query: 'GO',
          planner_type: planner,
          exclude_tests: planner === 'cluster' ? excludeTests : null,
        },
      });
      const id = invocationIdFrom(
        unwrapBody(response),
        t('deepwiki.generate.noInvocation', 'The provider accepted the request but returned no invocation to follow.'),
      );
      storage.save({ invocationId: id, startedAt: Date.now() });
      setInvocationId(id);
    } catch (cause) {
      setStartError(cause instanceof Error ? cause.message : String(cause));
    }
  }, [excludeTests, planner, projectId, settings, storage]);

  return (
    <Stack sx={{ gap: 1 }} data-testid="wiki-generation-panel">
      <Stack sx={{ flexDirection: 'row', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
        <Chip
          size="small"
          variant="outlined"
          data-testid="wiki-slots"
          label={slotsLabel(slotsBody)}
        />
        <ToggleButtonGroup exclusive size="small" value={planner} onChange={(_e, v: PlannerMode | null) => { if (v) setPlanner(v); }} aria-label={t('deepwiki.generate.planner', 'Planner')}>
          <ToggleButton value="deepagents">{t('deepwiki.generate.plannerAgentic', 'Agentic')}</ToggleButton>
          <ToggleButton value="cluster">{t('deepwiki.generate.plannerCluster', 'Clustering')}</ToggleButton>
        </ToggleButtonGroup>
        {planner === 'cluster' ? (
          <FormControlLabel
            control={<Switch size="small" checked={excludeTests} onChange={(_e, v) => { setExcludeTests(v); }} />}
            label={t('deepwiki.generate.excludeTests', 'Skip tests')}
          />
        ) : null}
        <BaseBtn
          variant="elitea"
          size="small"
          disabled={running || !canStart}
          data-testid="wiki-generate"
          onClick={() => { if (hasWiki) setConfirmOpen(true); else void start(); }}
        >
          {generateLabel(running, canStart)}
        </BaseBtn>
        {running ? (
          <BaseBtn variant="alarm" size="small" onClick={() => void stop()} loading={stopping} data-testid="wiki-generate-stop">
            {t('deepwiki.generate.stop', 'Stop generation')}
          </BaseBtn>
        ) : null}
      </Stack>

      {startError === null ? null : (
        <Alert severity="error" data-testid="wiki-generate-error">{startError}</Alert>
      )}

      {invocationId === null ? null : (
        <Stack sx={{ gap: 0.5 }} data-testid="wiki-generation-log">
          <Typography variant="bodySmall" data-testid="wiki-generation-status" data-status={generation.status.status}>
            {generation.status.message}
          </Typography>
          {generation.thinkingSteps.map((step) => (
            <Typography key={step.id} variant="bodySmall" color="text.secondary">
              {step.message}
            </Typography>
          ))}
        </Stack>
      )}

      <BaseModal
        open={confirmOpen}
        onClose={() => { setConfirmOpen(false); }}
        title={t('deepwiki.generate.confirmTitle', 'Generate wiki documentation?')}
        content={
          <Typography variant="bodyMedium">
            {t('deepwiki.generate.confirmBody', 'This regenerates all wiki documentation from the repository. It may take several minutes.')}
          </Typography>
        }
        actions={{
          node: (
            <>
              <BaseBtn variant="secondary" onClick={() => { setConfirmOpen(false); }}>
                {t('deepwiki.generate.confirmCancel', 'Cancel')}
              </BaseBtn>
              <BaseBtn variant="elitea" onClick={() => void start()} data-testid="wiki-generate-confirm">
                {t('deepwiki.generate.confirmStart', 'Generate')}
              </BaseBtn>
            </>
          ),
        }}
      />
    </Stack>
  );
}
