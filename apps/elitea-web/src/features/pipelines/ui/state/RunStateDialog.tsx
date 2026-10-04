/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * state/RunStateDialog.jsx` (755 lines) — unit A2j. A run's full timeline
 * viewer: a status header, a step-by-step `Stepper`, and a before/after
 * accordion per state variable at the selected step. Sub-components live in
 * `./RunStateDialog.parts.tsx`, styles in `./RunStateDialog.styles.ts` (both
 * purely to keep this file under the §3.5 400-line/12-prop budgets — this
 * file alone is the single biggest old-app file in this sub-unit).
 *
 * CROSS-SUB-UNIT CONTRACT (already consumed): `ui/nodes/RunStateNode.tsx`
 * (landed, unit A2f) imports `RunStateDialog` from this exact path/named
 * export and calls it with `data: RunStateNodeData` (`{status, label,
 * [key: string]: unknown}` — see that file's own doc comment) and
 * `state: YamlPipelineDocument['state']`. `RunStateDialogProps.data` below
 * matches `RunStateNodeData`'s shape exactly (not the more specific
 * `RunPipelineStatus['data']` shape from `lib/flow-editor/helpers/
 * parseRunsByEvent.support.ts`, which has a properly-typed `timeline:
 * RunTimelineEntry[]` but is NOT what the landed caller actually passes) —
 * `timeline` is read off the loose `data` prop via a narrowing cast at the
 * point of use, matching the baseline's own untyped assumption
 * (`data.timeline`) exactly. `onStop`/`onDelete`'s `{ stopPropagation }`
 * parameter type also matches `RunStateNode.tsx`'s own local `onStop`/
 * `onDelete` callbacks verbatim.
 *
 * `StyledTooltip` (baseline: `@/ComponentsLib/Tooltip`) -> MUI's `Tooltip`
 * directly, the same substitution `ui/nodes/DecisionNode/
 * DecisionNodeShared.tsx`/`ui/nodes/RunStateNode.tsx` already establish.
 * `DeleteIcon`/`CollapseIcon`/`StopIcon`/`AttentionIcon` (baseline) map to
 * this app's already-ported `shared/ui/icons/*` set 1:1 except `DeleteIcon`
 * (custom SVG, not ported — `@mui/icons-material`'s `DeleteOutlined`, same
 * already-established substitute). `StyledCircleProgress` (baseline:
 * `@/components/Chat/StyledComponents`, a themed `CircularProgress` wrapper
 * not part of this unit's scope) -> plain MUI `CircularProgress` — its only
 * customization was `size`, passed through identically either way.
 * `AccordionConstants.AccordionShowMode.LeftMode` -> omitted entirely:
 * `shared/ui/BasicAccordion`'s `showMode` already defaults to `'left'`.
 */
import type { KeyboardEvent as ReactKeyboardEvent, ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import { format } from 'date-fns';

import Box from '@mui/material/Box';
import Dialog from '@mui/material/Dialog';
import DialogContent from '@mui/material/DialogContent';
import Step from '@mui/material/Step';
import Stepper from '@mui/material/Stepper';
import Typography from '@mui/material/Typography';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { BasicAccordion, type AccordionItem } from '@/shared/ui/BasicAccordion';
import { t } from '@/shared/i18n';

import { ProcessConnector, ProcessStepIcon, RunStatus, StateItemView, StateValueModal } from './RunStateDialog.parts';
import { HeaderActions, StatusIndicatorRow } from './RunStateDialog.status';
import { isRunFinished, stepStateValues } from './runStateValues';
import {
  contentContainerSx,
  dialogContentSx,
  dialogPaperSx,
  headerActionsSx,
  headerSx,
  mainContainerSx,
  statesContainerSx,
  statesHeaderSx,
  stepLabelSx,
  stepperSx,
  stepSx,
  timelineHeaderSx,
  timelineStepSx,
  accordionSx as accordionItemSx,
  accordionSummarySx,
  accordionDetailsSx,
} from './RunStateDialog.styles';

/** @public Matches `ui/nodes/RunStateNode.tsx`'s `RunStateNodeData` exactly — see this file's own doc comment. */
export interface RunStateDialogData {
  readonly status: string;
  readonly label: string;
  readonly [key: string]: unknown;
}

interface RunStateTimelineStep {
  readonly id?: string;
  readonly status?: string;
  readonly created_at: number | string;
  readonly state?: Readonly<Record<string, unknown>>;
}

function timelineOf(data: RunStateDialogData): readonly RunStateTimelineStep[] {
  return (data['timeline'] as readonly RunStateTimelineStep[] | undefined) ?? [];
}

/** @public */
export interface RunStateDialogProps {
  readonly data: RunStateDialogData;
  readonly state: Readonly<Record<string, unknown>> | undefined;
  readonly open: boolean;
  readonly onClose: () => void;
  readonly onStop: (event: { readonly stopPropagation: () => void }) => void;
  readonly onDelete: (event: { readonly stopPropagation: () => void }) => void;
  readonly editorHeight?: number | undefined;
  readonly editorWidth?: number | undefined;
}

interface SelectedStateValue {
  readonly name: string;
  readonly value: unknown;
}

export function RunStateDialog(props: RunStateDialogProps): ReactNode {
  const { data, state, open, onClose, onStop, onDelete, editorHeight = 0, editorWidth = 0 } = props;

  const [selectedStep, setSelectedStep] = useState(0);
  const [showValueModal, setShowValueModal] = useState(false);
  const [selectedState, setSelectedState] = useState<SelectedStateValue | undefined>(undefined);

  const timeline = timelineOf(data);
  const variables = useMemo(() => Object.keys(state ?? { input: '', messages: [] }), [state]);

  const onSelect = useCallback((index: number) => setSelectedStep(index), []);

  const onFullScreen = useCallback((name: string, value: unknown) => {
    setShowValueModal(true);
    setSelectedState({ name, value });
  }, []);

  const onCloseValueModal = useCallback(() => setShowValueModal(false), []);

  useEffect(() => {
    if (data.status === FlowEditorConstants.PipelineStatus.InProgress && timeline.length) {
      setSelectedStep(timeline.length - 1);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [timeline.length]);

  const handleKeyDown = (event: ReactKeyboardEvent): void => {
    if (event.key === 'Escape') {
      event.preventDefault();
      onClose();
    }
  };

  const isError = data.status === FlowEditorConstants.PipelineStatus.Error;
  const runFinished = isRunFinished(data.status);
  const lastStep = timeline[timeline.length - 1];
  const timelineStepLabel =
    data.status === FlowEditorConstants.PipelineStatus.InProgress
      ? (timeline[timeline.length - 2]?.id ?? t('pipelines.flowEditor.state.start', 'Start'))
      : timeline[selectedStep]?.id;

  const showStatusIndicator =
    data.status === FlowEditorConstants.PipelineStatus.InProgress ||
    data.status === FlowEditorConstants.PipelineStatus.Error ||
    data.status === FlowEditorConstants.PipelineStatus.Interrupt;

  return (
    <>
      <Dialog
        open={open}
        onClose={onClose}
        onKeyDown={handleKeyDown}
        slotProps={{ paper: { sx: dialogPaperSx(editorWidth, editorHeight) } }}
      >
        <DialogContent sx={dialogContentSx}>
          <Box sx={mainContainerSx}>
            <Box sx={headerSx}>
              <Typography
                variant="labelMedium"
                color="text.secondary"
              >
                {data.label}
              </Typography>
              <Box sx={headerActionsSx}>
                <RunStatus status={data.status} />
                <HeaderActions
                  status={data.status}
                  onStop={onStop}
                  onDelete={onDelete}
                  onClose={onClose}
                />
              </Box>
            </Box>
            <Box sx={contentContainerSx}>
              <Box sx={timelineHeaderSx}>
                <Box sx={timelineStepSx}>
                  <Typography
                    variant="subtitle"
                    color="text.primary"
                  >
                    {t('pipelines.flowEditor.state.timelineStep', 'Timeline step:')}
                  </Typography>
                  <Typography
                    variant="bodyMedium"
                    color="text.secondary"
                  >
                    {timelineStepLabel}
                  </Typography>
                </Box>
                <StatusIndicatorRow
                  visible={showStatusIndicator}
                  lastStep={lastStep}
                />
              </Box>
              <Stepper
                sx={stepperSx}
                activeStep={timeline.findIndex((step) => step.status === FlowEditorConstants.PipelineStatus.InProgress)}
                connector={<ProcessConnector isError={isError} />}
              >
                {timeline.map((step, index) => (
                  // Keyed by position, matching the baseline
                  // (`RunStateDialog.jsx:380`, `key={index}`) exactly — NOT
                  // `step.id`. A pipeline node re-executing (e.g. inside a
                  // loop) produces multiple timeline entries with the same
                  // `id`; keying by `id` collides React's list reconciliation
                  // (duplicate keys), corrupting which entry's highlight/
                  // tooltip/DOM node is shown. `timeline` is append-only and
                  // rendered in that same fixed order, so the array index is
                  // a stable per-entry identity here.
                  <Step
                    key={index}
                    sx={stepSx}
                  >
                    <ProcessStepIcon
                      onSelect={onSelect}
                      index={index}
                      tooltip={step.id ?? ''}
                      active={index === selectedStep}
                      isError={isError}
                    />
                    <Typography
                      sx={stepLabelSx}
                      variant="bodySmall"
                    >
                      {formatStepTime(step.created_at)}
                    </Typography>
                  </Step>
                ))}
                <ProcessConnector
                  isError={isError}
                  visible={timeline.length < 2}
                />
              </Stepper>
              <Box sx={statesHeaderSx}>
                <Typography
                  variant="subtitle"
                  color="text.secondary"
                >
                  {t('pipelines.flowEditor.state.states', 'States')}
                </Typography>
              </Box>
              <Box sx={statesContainerSx}>
                {variables.map((variable, index) => {
                  // #6883: the last step of a finished (terminal) run shows the run's
                  // final state — see `runStateValues.ts`.
                  const values = stepStateValues(timeline, selectedStep, variable, runFinished);
                  const items: readonly AccordionItem[] = [
                    {
                      title: variable,
                      content: (
                        <StateItemView
                          name={variable}
                          onFullScreen={onFullScreen}
                          valueBefore={values.before}
                          valueAfter={values.after}
                          isFinal={values.isFinal}
                        />
                      ),
                    },
                  ];
                  return (
                    <BasicAccordion
                      key={variable}
                      items={items}
                      defaultExpanded={index === 0}
                      slotSx={{
                        accordion: accordionItemSx,
                        summary: accordionSummarySx,
                        title: { color: 'text.secondary' },
                        details: accordionDetailsSx,
                      }}
                    />
                  );
                })}
              </Box>
            </Box>
          </Box>
        </DialogContent>
      </Dialog>
      <StateValueModal
        open={showValueModal}
        onClose={onCloseValueModal}
        label={selectedState?.name}
        value={selectedState?.value}
      />
    </>
  );
}

/** Ported verbatim: `format(new Date(step.created_at), 'HH:mm:ss')` (baseline, `date-fns` — already a real dependency of this app, confirmed via `package.json` and used elsewhere, e.g. `features/notifications/ui/NotificationListItem.tsx`). */
function formatStepTime(createdAt: number | string): string {
  return format(new Date(createdAt), 'HH:mm:ss');
}
