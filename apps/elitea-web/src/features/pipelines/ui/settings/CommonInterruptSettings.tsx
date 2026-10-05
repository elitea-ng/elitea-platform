import type { ChangeEvent, ReactNode } from 'react';
import { useCallback, useContext, useMemo } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';

import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import * as FlowEditorHelpers from '../../lib/flow-editor/helpers/flowEditor.helpers';
import type { YamlPipelineDocument, YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * settings/CommonInterruptSettings.jsx` (unit A2h). Reads `FlowEditorContext`
 * from `../../lib/flow-editor/flowEditorContext.ts` (see `InputSelect.tsx`'s
 * doc comment for the R-L1 rationale).
 *
 * `styled(FormControlLabel)` (baseline: `@emotion/styled`) -> a plain `sx`
 * object -- this app's `shared/ui` components consistently use MUI's `sx`
 * prop rather than `@emotion/styled` wrappers for one-off styling.
 *
 * The native compiler admits before and after pauses for stored nodes.
 * The admission gate validates their identifiers and list limits.
 * Entry nodes and END-transition nodes support these pauses.
 */

export interface CommonInterruptSettingsProps {
  readonly id: string;
  readonly showStructuredOutput?: boolean;
  readonly type?: string;
  readonly disabled?: boolean | undefined;
}

const rowSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.5rem', width: '100%', flexDirection: 'row' };

function switchLabelSx(theme: Theme) {
  return {
    width: '13.375rem',
    height: '2rem',
    borderRadius: theme.vars.shape.radiusMd,
    marginLeft: '0rem',
    marginRight: '0rem',
    padding: '0.25rem 0.5rem',
    justifyContent: 'flex-start',
    gap: '0.5rem',
    background: theme.vars.palette.background.userInputBackground,
  };
}

interface InterruptSwitchRowProps {
  readonly disabled: boolean;
  readonly checked: boolean;
  readonly onChange?: ((event: ChangeEvent<HTMLInputElement>) => void) | undefined;
  readonly label: ReactNode;
  readonly emphasized: boolean;
}

function InterruptSwitchRow({ disabled, checked, onChange, label, emphasized }: InterruptSwitchRowProps): ReactNode {
  return (
    <FormControlLabel
      sx={switchLabelSx}
      control={
        <BaseSwitch
          disabled={disabled}
          checked={checked}
          onChange={onChange}
        />
      }
      label={
        <Typography
          variant="labelSmall"
          color={emphasized ? 'text.primary' : 'text.secondary'}
        >
          {label}
        </Typography>
      }
      labelPlacement="end"
    />
  );
}

interface InterruptSettingsContext {
  readonly yamlJsonObject: YamlPipelineDocument | undefined;
  readonly setYamlJsonObject: FlowEditorContextValue['setYamlJsonObject'] | undefined;
  readonly yamlNode: YamlPipelineNode | undefined;
  readonly setFlowEdges: FlowEditorContextValue['setFlowEdges'] | undefined;
  readonly realInterruptBefore: readonly string[];
  readonly realInterruptAfter: readonly string[];
}

/**
 * Baseline's `Array.isArray(yamlJsonObject?.interrupt_before) ?
 * yamlJsonObject?.interrupt_before : []` (`CommonInterruptSettings.jsx:27-34`)
 * -- a malformed (non-array) `interrupt_before`/`interrupt_after` value in
 * the YAML degrades to an empty list instead of flowing into `.includes()`.
 *
 * The explicitly-typed intermediate `list: readonly string[]` binding is
 * required, not stylistic: `Array.isArray`'s `arg is any[]` predicate widens
 * the narrowed value to `any[]`, tripping `no-unsafe-return` on a direct or
 * unannotated return -- the same gap `parsePipelineTraversal.helpers.ts:138-139`
 * and `yamlUpdate.helpers.ts` already document for this exact pattern.
 */
function toInterruptList(value: readonly string[] | undefined): readonly string[] {
  const list: readonly string[] = Array.isArray(value) ? value : [];
  return list;
}

/** Read the stored node and its pause lists from the editor context. */
function useInterruptSettingsContext(id: string, type: string | undefined): InterruptSettingsContext {
  const context = useContext(FlowEditorContext);
  const yamlJsonObject = context?.yamlJsonObject;

  const yamlNode = useMemo(() => yamlJsonObject?.nodes?.find((node) => matchesNode(node, id, type)), [id, type, yamlJsonObject]);

  return {
    yamlJsonObject,
    setYamlJsonObject: context?.setYamlJsonObject,
    yamlNode,
    setFlowEdges: context?.setFlowEdges,
    realInterruptBefore: toInterruptList(yamlJsonObject?.interrupt_before),
    realInterruptAfter: toInterruptList(yamlJsonObject?.interrupt_after),
  };
}

function matchesNode(node: YamlPipelineNode, id: string, type: string | undefined): boolean {
  return node.id === id && node.type === type;
}

export function CommonInterruptSettings(props: CommonInterruptSettingsProps): ReactNode {
  const { id, showStructuredOutput = true, type, disabled = false } = props;

  const { yamlJsonObject, setYamlJsonObject, setFlowEdges, yamlNode, realInterruptBefore, realInterruptAfter } = useInterruptSettingsContext(id, type);

  const onChangeStructuredOutput = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      if (!setYamlJsonObject) return;
      FlowEditorHelpers.updateYamlNode(id, 'structured_output', event.target.checked, yamlJsonObject, setYamlJsonObject);
    },
    [yamlJsonObject, setYamlJsonObject, id],
  );

  const updateInterrupt = useCallback(
    (field: 'interrupt_before' | 'interrupt_after', checked: boolean) => {
      if (!setYamlJsonObject || !yamlJsonObject || !yamlNode) return;
      const oldList = field === 'interrupt_before' ? realInterruptBefore : realInterruptAfter;
      const nextList = oldList.filter((nodeId) => nodeId !== id);
      if (checked) nextList.push(id);
      const before = field === 'interrupt_before' ? nextList : realInterruptBefore;
      const after = field === 'interrupt_after' ? nextList : realInterruptAfter;
      setYamlJsonObject({ ...yamlJsonObject, [field]: nextList });
      setFlowEdges?.((edges) => edges.map((edge) => {
        const affected = field === 'interrupt_before' ? edge.target === id : edge.source === id;
        if (!affected) return edge;
        const data = { ...edge.data };
        if (before.includes(edge.target) || after.includes(edge.source)) data.label = 'interrupt';
        else delete data.label;
        return { ...edge, data };
      }));
    },
    [id, realInterruptBefore, realInterruptAfter, setFlowEdges, setYamlJsonObject, yamlJsonObject, yamlNode],
  );

  const onChangeInterruptBefore = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => updateInterrupt('interrupt_before', event.target.checked),
    [updateInterrupt],
  );
  const onChangeInterruptAfter = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => updateInterrupt('interrupt_after', event.target.checked),
    [updateInterrupt],
  );

  return (
    <Box sx={rowSx}>
      <InterruptSwitchRow
        disabled={disabled || !yamlNode}
        checked={realInterruptBefore.includes(id)}
        onChange={onChangeInterruptBefore}
        label={t('pipelines.commonInterruptSettings.interruptBefore', 'Interrupt before')}
        emphasized={false}
      />
      <InterruptSwitchRow
        disabled={disabled || !yamlNode}
        checked={realInterruptAfter.includes(id)}
        onChange={onChangeInterruptAfter}
        label={t('pipelines.commonInterruptSettings.interruptAfter', 'Interrupt after')}
        emphasized={false}
      />
      {showStructuredOutput && (
        <InterruptSwitchRow
          disabled={disabled}
          checked={Boolean(yamlNode?.structured_output)}
          onChange={onChangeStructuredOutput}
          label={t('pipelines.commonInterruptSettings.structuredOutput', 'Structured output')}
          emphasized={false}
        />
      )}
    </Box>
  );
}
