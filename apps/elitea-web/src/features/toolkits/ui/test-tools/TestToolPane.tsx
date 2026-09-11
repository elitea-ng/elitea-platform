/**
 * The toolkit editor's "Test settings" pane — pick a tool, fill its arguments,
 * run it, read what came back.
 *
 * ## What this replaces
 *
 * `pages/toolkits/lib/configurationTabSlots.tsx`'s `renderTestPane` returned an
 * empty `<Box data-testid="edit-toolkit-test-pane-slot" />`, and its own comment
 * said why: the ported `./TestTools.tsx` needs a live chat transcript, and with
 * it `features/chat` and a `widgets/`-layer model selector, neither of which
 * this app has. Both live-lane Test-Settings journeys
 * (`e2e/live/liveToolkits.ts`, `LIVE-TK-<provider>-1`) worked around the gap by
 * driving a chat turn instead, and stated in their own header that they were
 * testing something the legacy case was not about.
 *
 * ## Why this pane needs none of that
 *
 * The legacy panel's Run Tool button is a SYNCHRONOUS tool run, and this
 * platform serves one: `POST /elitea_core/test_tool/prompt_lib/{projectId}/
 * {toolId}` (`toolkits.Handler.TestTool`), which `../../api/toolkitTestRun.ts`
 * maps onto a closed union of outcomes. One request, one settled answer, no
 * model, no conversation, no socket. So the pane is the panel
 * (`./TestToolSettings.tsx`, already ported byte for byte) plus a result panel
 * (`./TestToolResultPanel.tsx`) — and it is an ordinary intra-slice component
 * with no cross-layer dependency at all, which is why `ConfigurationTab` can
 * render it directly instead of receiving it through a slot.
 *
 * `./TestTools.tsx` is NOT deleted or superseded. It is the fuller surface —
 * the transcript, the model, the MCP login — and it keeps its slot the day the
 * app grows what it needs. This pane is the part that can be built today, and
 * building it is what makes the credential and the tool list testable from the
 * screen that edits them.
 *
 * ## Disclosed narrowings against the legacy panel
 *
 *  1. **No model selector.** A synchronous tool run asks no model anything.
 *     `TestToolSettings`' `llm`/`LLMModelSelector` became optional for this.
 *  2. **`index_data` is offered but cannot run here.** The Go start handler
 *     admits `index_data` on a different, asynchronous route
 *     (`internal/api/v2/indexing/start_handler.go`), which the Indexes tab
 *     owns. Pressing Run on it here answers the route's own refusal rather
 *     than pretending; the honest message is the one the server writes.
 *  3. **No run history.** `ConfigurationTab` already renders the
 *     `ViewRunHistoryButton` above this pane, through its own slot.
 */
import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import type { ToolkitTestAuthorization } from '../../api/toolkitTestAuthorization';
import { t } from '@/shared/i18n';
import type { SxProps, Theme } from '@mui/material/styles';

import { ToolTypes } from '@/entities/toolkit';

import { IndexesToolsEnum } from '../../indexes/lib/constants/indexDetails.constants';
import { adjustIndexDataSchema } from '../../indexes/lib/helpers/indexChat.helpers';
import { useIndexNameValidation } from '../../indexes/lib/hooks/useIndexNameValidation.hooks';
import { validateToolkitForm } from '../../lib/helpers/toolkitChat.helpers';
import type { ToolFormSchema } from '../../lib/helpers/toolkitChat.helpers';
import type { ToolkitConversationValues } from '../../lib/helpers/toolkitConversation.helpers';

import { resolveDefaultValue } from './TestTools.helpers';
import { TestToolResultPanel } from './TestToolResultPanel';
import { TestToolSettings } from './TestToolSettings';
import type { McpToolOption } from './useGetSelectedToolSchema';
import { useGetSelectedToolSchema } from './useGetSelectedToolSchema';
import { useToolkitTestToolRun } from './useToolkitTestToolRun';

export interface ToolkitTestAuthorizationRenderProps {
  readonly projectId: string;
  readonly challenge: ToolkitTestAuthorization;
  readonly onAuthorized: (reference: string) => Promise<void>;
  readonly onSkip: () => void;
}

export interface TestToolPaneProps {
  readonly renderAuthorization?: ((props: ToolkitTestAuthorizationRenderProps) => ReactNode) | undefined;
  readonly projectId: string | number | undefined;
  readonly toolkitId: string | number | undefined;
  /** The toolkit being edited, as the form currently holds it — its `type` picks the tool catalogue and its `settings` the explicit tool list. */
  readonly values: ToolkitConversationValues;
}

export function TestToolPane({ projectId, toolkitId, values, renderAuthorization }: TestToolPaneProps): ReactNode {
  const [selectedTool, setSelectedTool] = useState<string | null>(null);
  const [toolInputVariables, setToolInputVariables] = useState<Record<string, unknown>>({});
  /** The tool whose schema defaults have already been applied, so they are applied once per pick. */
  const initializedToolRef = useRef<string | null>(null);

  const indexNameValidation = useIndexNameValidation();
  const { clearIndexNameError, indexNameError } = indexNameValidation;

  const {
    toolSchema,
    isError: toolSchemaReadFailed,
    refetch: retryToolSchemaRead,
  } = useGetSelectedToolSchema({
    projectId,
    toolkitId,
    toolkitType: values.type,
    toolOptionType: selectedTool,
    availableMcpTools: values.settings?.['available_mcp_tools'] as readonly McpToolOption[] | undefined,
  });

  const selectedToolSchema = useMemo(() => {
    if (selectedTool === IndexesToolsEnum.indexData) {
      return adjustIndexDataSchema(toolSchema, { index_name: indexNameError ? { error: indexNameError } : {} });
    }
    return toolSchema;
  }, [selectedTool, indexNameError, toolSchema]);

  const isValidForm = useMemo(() => {
    if (values.type === ToolTypes.custom.value) return true;
    if (!selectedTool || !selectedToolSchema?.properties) return false;
    // Same `exactOptionalPropertyTypes`-only cast `./TestTools.tsx` documents:
    // `required: readonly string[] | undefined` versus absent-or-array, and
    // `validateToolkitForm` reads `schema.required ?? []` either way.
    return validateToolkitForm(selectedToolSchema as ToolFormSchema, toolInputVariables);
  }, [selectedTool, toolInputVariables, selectedToolSchema, values.type]);

  const { outcome, isRunning, run, reset, authorize, skip } = useToolkitTestToolRun({ projectId, toolkitId });

  const onChangeInputVariables = useCallback((inputVariables: Readonly<Record<string, unknown>>) => {
    setToolInputVariables(inputVariables);
  }, []);

  const onChangeTool = useCallback(
    (value: string | null) => {
      setSelectedTool(value || null);
      setToolInputVariables({});
      initializedToolRef.current = null;
      // The previous tool's result must not sit under the new tool's arguments.
      reset();
    },
    [reset],
  );

  const onRunTool = useCallback(() => {
    if (selectedTool === null) return;
    void run(selectedTool, toolInputVariables);
  }, [run, selectedTool, toolInputVariables]);

  // The baseline's own default-filling pass, once per picked tool.
  useEffect(() => {
    const properties = selectedToolSchema?.properties;
    if (!properties || !selectedTool || initializedToolRef.current === selectedTool) return;
    initializedToolRef.current = selectedTool;

    const defaults: Record<string, unknown> = {};
    let hasDefaults = false;
    for (const [key, property] of Object.entries(properties)) {
      const current = toolInputVariables[key];
      if (current !== undefined && current !== '' && typeof current !== 'function') continue;
      const defaultValue = resolveDefaultValue(property);
      if (defaultValue !== undefined) {
        defaults[key] = defaultValue;
        hasDefaults = true;
      }
    }
    if (hasDefaults) setToolInputVariables((previous) => ({ ...previous, ...defaults }));
  }, [selectedTool, selectedToolSchema?.properties, toolInputVariables]);

  useEffect(() => {
    clearIndexNameError();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- baseline only clears on `selectedTool` change
  }, [selectedTool]);

  const toolSchemaRead = useMemo(
    () => ({ isError: toolSchemaReadFailed, onRetry: retryToolSchemaRead }),
    [toolSchemaReadFailed, retryToolSchemaRead],
  );

  return (
    <Box
      sx={rootSx}
      // The id `e2e/live/toolkits.indicators.spec.ts` waits on to know the
      // configuration tab has rendered. It was the empty slot's testid, and it
      // stays on the real pane so that wait keeps its meaning.
      data-testid="edit-toolkit-test-pane-slot"
    >
      <TestToolSettings
        projectId={projectId}
        toolkitId={toolkitId}
        selectedTool={selectedTool}
        onChangeTool={onChangeTool}
        toolInputVariables={toolInputVariables}
        onChangeInputVariables={onChangeInputVariables}
        onRunTool={onRunTool}
        isRunning={isRunning}
        isValidForm={isValidForm}
        selectedToolSchema={selectedToolSchema}
        values={values}
        indexNameValidation={indexNameValidation}
        toolSchemaRead={toolSchemaRead}
      />
      {outcome?.kind === 'authorizationRequired' && !isRunning && (
        renderAuthorization && projectId !== undefined
          ? renderAuthorization({ projectId: String(projectId), challenge: outcome.challenge, onAuthorized: authorize, onSkip: skip })
          : <Button onClick={skip}>{t('features.toolkits.testToolPane.skip', 'Skip')}</Button>
      )}
      <TestToolResultPanel
        outcome={outcome}
        isRunning={isRunning}
      />
    </Box>
  );
}

const rootSx: SxProps<Theme> = {
  flex: 1,
  minWidth: 0,
  display: 'flex',
  flexDirection: 'column',
  maxHeight: '100%',
  overflowY: 'auto',
};
