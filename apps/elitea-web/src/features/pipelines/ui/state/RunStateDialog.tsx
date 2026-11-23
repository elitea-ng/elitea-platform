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

import Box from '@mui/material/Box';
import Dialog from '@mui/material/Dialog';
import DialogContent from '@mui/material/DialogContent';
import Typography from '@mui/material/Typography';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import type { GroupedTimelineStep } from '../../lib/run-state-timeline';
import { BasicAccordion, type AccordionItem } from '@/shared/ui/BasicAccordion';
import { t } from '@/shared/i18n';

import { TimeLine } from '../run-state-timeline';
import { RunStatus, StateItemView, StateValueModal } from './RunStateDialog.parts';
import { HeaderActions } from './RunStateDialog.status';
import { isRunFinished, stepStateValues, type StepStateValues } from './runStateValues';
import {
  contentContainerSx,
  dialogContentSx,
  dialogPaperSx,
  headerActionsSx,
  headerSx,
  mainContainerSx,
  statesContainerSx,
  statesHeaderSx,
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

function timelineOf(data: RunStateDialogData): readonly GroupedTimelineStep[] {
  return (data['timeline'] as readonly GroupedTimelineStep[] | undefined) ?? [];
}

function groupedTimelineOf(data: RunStateDialogData): readonly GroupedTimelineStep[] | undefined {
  const grouped = data['groupedTimeline'] as readonly GroupedTimelineStep[] | undefined;
  return grouped && grouped.length > 0 ? grouped : undefined;
}

function lastOuterStepIndex(steps: readonly GroupedTimelineStep[]): number {
  for (let index = steps.length - 1; index >= 0; index -= 1) {
    if (!steps[index]?.is_nested) return index;
  }
  return steps.length - 1;
}

function previousScopedStep(
  steps: readonly GroupedTimelineStep[],
  selectedStep: number,
  currentStep: GroupedTimelineStep | undefined,
): GroupedTimelineStep | undefined {
  for (let index = selectedStep - 1; index >= 0; index -= 1) {
    const candidate = steps[index];
    if (!candidate) continue;
    if (currentStep?.is_nested) {
      if (candidate.is_nested && candidate.scope_id === currentStep.scope_id) return candidate;
    } else if (!candidate.is_nested) {
      return candidate;
    }
  }
  return undefined;
}

function resolveStepValues(
  steps: readonly GroupedTimelineStep[],
  selectedStep: number,
  variable: string,
  runFinished: boolean,
): StepStateValues {
  const currentStep = steps[selectedStep];
  if (currentStep?.is_nested) {
    const prev = previousScopedStep(steps, selectedStep, currentStep);
    return {
      before: prev ? prev.state?.[variable] : '',
      after: currentStep.state?.[variable],
      isFinal: false,
    };
  }
  return stepStateValues(steps, selectedStep, variable, runFinished);
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

  const outerSteps = timelineOf(data);
  const steps = groupedTimelineOf(data) ?? outerSteps;
  const lastOuterIndex = lastOuterStepIndex(steps);
  const variables = useMemo(() => Object.keys(state ?? { input: '', messages: [] }), [state]);

  const onSelect = useCallback((index: number) => setSelectedStep(index), []);

  const onFullScreen = useCallback((name: string, value: unknown) => {
    setShowValueModal(true);
    setSelectedState({ name, value });
  }, []);

  const onCloseValueModal = useCallback(() => setShowValueModal(false), []);

  useEffect(() => {
    if (data.status === FlowEditorConstants.PipelineStatus.InProgress && lastOuterIndex >= 0) {
      setSelectedStep(lastOuterIndex);
    }
  }, [data.status, lastOuterIndex]);

  const handleKeyDown = (event: ReactKeyboardEvent): void => {
    if (event.key === 'Escape') {
      event.preventDefault();
      onClose();
    }
  };

  const runFinished = isRunFinished(data.status);

  return (
    <>
      <Dialog
        open={open}
        onClose={onClose}
        onKeyDown={handleKeyDown}
        slotProps={{ paper: { sx: dialogPaperSx(editorWidth, editorHeight) } }}
      >
        <DialogContent
          sx={dialogContentSx}
          data-testid="pipeline-run-details-panel"
        >
          <Box sx={mainContainerSx}>
            <Box sx={headerSx}>
              <Typography
                variant="labelMedium"
                color="text.secondary"
                data-testid="pipeline-run-details-header"
              >
                {data.label}
              </Typography>
              <Box sx={headerActionsSx}>
                <RunStatus status={data.status} />
                <HeaderActions
                  status={data.status}
                  recoveryPaused={data['recoveryPaused'] === true && data.status === FlowEditorConstants.PipelineStatus.Interrupt}
                  onStop={onStop}
                  onDelete={onDelete}
                  onClose={onClose}
                />
              </Box>
            </Box>
            <Box sx={contentContainerSx}>
              <TimeLine
                key={data.label}
                steps={steps}
                outerSteps={outerSteps}
                status={data.status}
                selectedStep={selectedStep}
                onSelect={onSelect}
              />
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
                  const values = resolveStepValues(steps, selectedStep, variable, runFinished);
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
