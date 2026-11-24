import type { Component, ErrorInfo, ReactNode } from 'react';
import {
  PureComponent,
  Suspense,
  forwardRef,
  lazy,
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useRef,
  useState,
} from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';
import { useRouterState } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { PipelineEditorMode } from '@/shared/lib/enums';
import type { CodeMirrorEditorHandle } from '@/shared/ui/CodeMirrorEditor';
import { RefreshIcon } from '@/shared/ui/icons/refresh-icon';
import { TabGroupButton } from '@/shared/ui/TabGroupButton';
import type { TabGroupButtonItem } from '@/shared/ui/TabGroupButton';
import { combineSx } from '@/shared/ui/lib/combineSx';

import type { AiAssistantLlmSettings } from '../api/aiAssistantPredict';
import { useIsPipelineYamlCodeDirty, isChatPath } from '../lib/hooks/useIsPipelineYamlCodeDirty';
import { useIsSmallWindow } from '../lib/hooks/useIsSmallWindow';
import { usePipelineYamlSerialization } from '../lib/hooks/usePipelineYamlSerialization';
import { parseYaml } from '../lib/flow-editor/helpers/parsePipeline.helpers';
import { getTestBlockReason } from '../lib/flow-editor/helpers/pipelineReadiness.helpers';
import type { RunSocketEvent } from '../lib/flow-editor/helpers/parseRunsByEvent.support';
import type { FlowEdge, FlowNode, SetYamlJsonObject } from '../lib/flow-editor/reactFlowTypes';
import type { YamlPipelineDocument } from '../lib/flow-editor/helpers/pipelineFlow.types';
import { usePipelineYamlStore } from '../model/pipelineYamlStore';
import { AddNodeMenu } from './AddNodeMenu';
import type { PipelineNodeType } from '../lib/flow-editor/constants/flowEditor.constants';
import type { FlowEditorHandle } from './FlowEditor';
import type { PipelineToolEntry } from './select/pipelineToolEntry.types';
import { PipelineYamlSerializationAlert } from './PipelineYamlSerializationAlert';
import { TestPipelineButton } from './TestPipelineButton';
import { YamlCodeEditor } from './YamlCodeEditor';
import type { PipelineEditorModeValue } from './FlowWrapper';

const FlowWrapperLazy = lazy(() => import('./FlowWrapper').then((m) => ({ default: m.FlowWrapper })));

/** Ported from `EliteaUI` `pages/Pipelines/Components/EditorPanel.jsx` — Flow/YAML toolbar + lazy FlowWrapper + YamlCodeEditor. */
export interface EditorPanelHandle {
  readonly onRcvAgentEvent: (event: unknown) => void;
  readonly deleteAllRunNodes: () => void;
  readonly fitView: () => void;
  readonly onStopRun: () => void;
  readonly hasRunsInProgress: () => boolean;
}

export interface EditorPanelProps {
  readonly setYamlDirty: (dirty: boolean) => void;
  readonly stopRun: () => void;
  readonly display?: string | undefined;
  readonly sx?: SxProps<Theme> | undefined;
  readonly disabled?: boolean | undefined;
  readonly versionTools?: readonly PipelineToolEntry[] | undefined;
  readonly llmSettings?: AiAssistantLlmSettings | null | undefined;
  readonly onTestPipeline?: (() => void) | undefined;
  readonly testDisabledReason?: string | undefined;
  readonly isTestPanelOpen?: boolean | undefined;
  readonly onCanvasNodeInteraction?: (() => void) | undefined;
}

interface FlowEditorErrorBoundaryProps {
  readonly display: string;
  readonly children: ReactNode;
}
interface FlowEditorErrorBoundaryState {
  readonly hasError: boolean;
}

/** Local error boundary — see module doc comment for why this replaces the baseline's `react-error-boundary` dependency. */
class FlowEditorErrorBoundary extends PureComponent<FlowEditorErrorBoundaryProps, FlowEditorErrorBoundaryState> implements Component {
  override state: FlowEditorErrorBoundaryState = { hasError: false };

  static getDerivedStateFromError(): FlowEditorErrorBoundaryState {
    return { hasError: true };
  }

  override componentDidCatch(_error: Error, _info: ErrorInfo): void {
    // Intentionally silent beyond the fallback UI below — no error-reporting sink exists in this app yet.
  }

  override render(): ReactNode {
    if (this.state.hasError) {
      return (
        <Box sx={errorFallbackSx(this.props.display)}>
          <Typography
            variant="headingSmall"
            sx={errorTitleSx}
          >
            {t('features.pipelines.editorPanel.errorBoundary.title', 'Failed to load the flow editor')}
          </Typography>
          <Typography
            variant="bodyMedium"
            color="text.primary"
            sx={errorMessageSx}
          >
            {t(
              'features.pipelines.editorPanel.errorBoundary.message',
              'The visual flow editor may have been updated or encountered an error and could not be loaded. Please try reloading the page or switch to YAML mode.',
            )}
          </Typography>
          <Tooltip
            title={t('features.pipelines.editorPanel.errorBoundary.reloadTooltip', 'Reload page')}
            placement="top" describeChild
          >
            <IconButton onClick={() => window.location.reload()}>
              <RefreshIcon style={refreshIconStyle} />
              <Typography
                variant="labelSmall"
                color="text.secondary"
              >
                {t('features.pipelines.editorPanel.errorBoundary.reloadLabel', 'Reload the page')}
              </Typography>
            </IconButton>
          </Tooltip>
        </Box>
      );
    }
    return this.props.children;
  }
}

const headerContainerSx = (isFromChat: boolean): SxProps<Theme> => ({
  display: 'flex',
  justifyContent: 'space-between',
  alignItems: 'center',
  marginTop: '0.25rem',
  marginBottom: '1rem',
  paddingRight: isFromChat ? '0.5rem' : undefined,
  paddingLeft: isFromChat ? '0.5rem' : undefined,
});
const actionButtonsContainerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem' };
const editorContainerSx: SxProps<Theme> = { flex: 1, display: 'flex', flexDirection: 'column', position: 'relative', overflow: 'hidden' };
const errorFallbackSx = (display: string): SxProps<Theme> => ({
  display: display === PipelineEditorMode.Flow ? 'flex' : 'none',
  flexDirection: 'column',
  justifyContent: 'center',
  alignItems: 'center',
  flex: 1,
  padding: '2rem',
  backgroundColor: ({ vars }) => vars.palette.background.paper,
  border: ({ vars }) => `0.0625rem solid ${vars.palette.border.lines}`,
  borderRadius: ({ vars }) => vars.shape.radiusMd,
  gap: '1rem',
});
const errorTitleSx: SxProps<Theme> = { textAlign: 'center' };
const errorMessageSx: SxProps<Theme> = { textAlign: 'center', maxWidth: '25rem' };
const refreshIconStyle = { width: '1rem', height: '1rem' };
const yamlEditorContainerSx = (isFromChat: boolean, isSmallWindow: boolean): SxProps<Theme> => ({
  width: '100%',
  padding: '0.125rem',
  boxSizing: 'border-box',
  flex: 1,
  border: ({ vars }) => (isFromChat ? 'none' : `0.0625rem solid ${vars.palette.border.lines}`),
  borderTop: ({ vars }) => (isFromChat ? `0.0625rem solid ${vars.palette.border.lines}` : undefined),
  borderRadius: isFromChat ? '0' : '0.5rem',
  height: 'calc(100% - 2.5rem)',
  minHeight: isSmallWindow ? 'calc(100vh - 13.75rem)' : undefined,
});

export const EditorPanel = forwardRef<EditorPanelHandle, EditorPanelProps>(function EditorPanel(props, ref): ReactNode {
  const {
    setYamlDirty,
    stopRun,
    display,
    sx,
    disabled,
    versionTools,
    llmSettings,
    onTestPipeline,
    testDisabledReason,
    isTestPanelOpen = false,
    onCanvasNodeInteraction,
  } = props;
  const { isSmallWindow } = useIsSmallWindow();
  const pathname = useRouterState({ select: (routerState) => routerState.location.pathname });
  const isFromChat = isChatPath(pathname);
  const [mode, setMode] = useState<PipelineEditorModeValue>(PipelineEditorMode.Flow);
  const flowEditorRef = useRef<FlowEditorHandle>(null);
  const yamlEditorRef = useRef<CodeMirrorEditorHandle>(null);

  const isYamlCodeDirty = useIsPipelineYamlCodeDirty();
  useEffect(() => {
    setYamlDirty(isYamlCodeDirty);
  }, [isYamlCodeDirty, setYamlDirty]);

  const yamlCode = usePipelineYamlStore((state) => state.yamlCode);
  const yamlJsonObject = usePipelineYamlStore((state) => state.yamlJsonObject);
  const resetFlag = usePipelineYamlStore((state) => state.resetFlag);
  const layoutVersion = usePipelineYamlStore((state) => state.layoutVersion);
  const storeSetYamlCode = usePipelineYamlStore((state) => state.setYamlCode);
  const editPipelineYamlDocument = usePipelineYamlStore((state) => state.editPipelineYamlDocument);
  const parsePipelineYamlCode = usePipelineYamlStore((state) => state.parsePipelineYamlCode);
  const clearResetFlag = usePipelineYamlStore((state) => state.clearResetFlag);
  const setLayoutVersion = usePipelineYamlStore((state) => state.setLayoutVersion);

  const { serializationError, serializeDocument, reportSerializationError } = usePipelineYamlSerialization(
    yamlCode,
    yamlJsonObject,
  );

  const setYamlJsonObject = useCallback<SetYamlJsonObject>(
    (next, options) => {
      try {
        editPipelineYamlDocument(next, options);
        return true;
      } catch (caught) {
        reportSerializationError(caught);
        return false;
      }
    },
    [editPipelineYamlDocument, reportSerializationError],
  );

  const onParseCodeToJson = useCallback(
    (code: string) => {
      try {
        if (parsePipelineYamlCode(code)) {
          const parsed = usePipelineYamlStore.getState().yamlJsonObject as YamlPipelineDocument;
          const expanded = flowEditorRef.current?.getCurrentExpandState();
          flowEditorRef.current?.calculateLayoutNodes(parsed, true, true, expanded);
        }
      } catch {
        // Invalid text remains in the YAML editor; keep the last valid flow view.
      }
    },
    [parsePipelineYamlCode],
  );

  const onChangeCode = useCallback(
    (code: string) => {
      storeSetYamlCode(code);
    },
    [storeSetYamlCode],
  );

  const onRcvAgentEvent = useCallback((event: unknown) => {
    flowEditorRef.current?.onRcvAgentEvent(event as RunSocketEvent);
  }, []);

  const deleteAllRunNodes = useCallback(() => {
    flowEditorRef.current?.deleteAllRunNodes();
  }, []);

  const fitView = useCallback(() => {
    flowEditorRef.current?.fitView();
  }, []);

  useImperativeHandle(
    ref,
    () => ({
      onRcvAgentEvent,
      deleteAllRunNodes,
      fitView,
      onStopRun: () => flowEditorRef.current?.stopCurrentRun(),
      hasRunsInProgress: () => flowEditorRef.current?.hasRunsInProgress() ?? false,
    }),
    [onRcvAgentEvent, deleteAllRunNodes, fitView],
  );

  const buttonItems = useMemo<readonly TabGroupButtonItem[]>(
    () => [
      { label: t('features.pipelines.editorPanel.mode.flow', 'Flow'), value: PipelineEditorMode.Flow },
      { label: t('features.pipelines.editorPanel.mode.yaml', 'YAML'), value: PipelineEditorMode.Yaml },
    ],
    [],
  );

  const onSelectChatMode = useCallback(
    (newMode: string) => {
      if (mode === newMode) return;
      if (newMode === PipelineEditorMode.Flow) {
        // Commit the live document before unmount cancels its debounced change notification.
        const storedCode = usePipelineYamlStore.getState().yamlCode;
        const currentCode = yamlEditorRef.current?.getCode() ?? storedCode;
        // CodeMirror normalizes line endings. Preserve stored bytes when the document is unchanged.
        const code = currentCode === storedCode.replace(/\r\n?/g, '\n') ? storedCode : currentCode;
        storeSetYamlCode(code);
        onParseCodeToJson(code);
      } else {
        const yamlString = serializeDocument(yamlJsonObject, { originalYaml: yamlCode });
        if (yamlString !== undefined && Object.keys(yamlJsonObject).length && yamlString !== yamlCode)
          storeSetYamlCode(yamlString);
      }
      setMode(newMode as PipelineEditorModeValue);
    },
    [mode, onParseCodeToJson, serializeDocument, storeSetYamlCode, yamlJsonObject, yamlCode],
  );

  const onAddNode = useCallback((type: PipelineNodeType) => void flowEditorRef.current?.onAddNode(type), []);

  useEffect(() => {
    const nodes = (yamlJsonObject as { readonly nodes?: readonly { readonly decision?: unknown }[] }).nodes;
    const yamlString = nodes?.find((node) => node.decision) ? serializeDocument(yamlJsonObject, { originalYaml: yamlCode }) : undefined;
    if (yamlString !== undefined) onParseCodeToJson(yamlString);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [yamlJsonObject]);

  const initialNodes: readonly FlowNode[] = useMemo(() => parseYaml(yamlJsonObject).nodes as unknown as readonly FlowNode[], [yamlJsonObject]);
  const initialEdges: readonly FlowEdge[] = useMemo(() => parseYaml(yamlJsonObject).edges as unknown as readonly FlowEdge[], [yamlJsonObject]);

  const flowTestDisabledReason = useMemo(
    () => (onTestPipeline ? getTestBlockReason(yamlJsonObject) : ''),
    [onTestPipeline, yamlJsonObject],
  );
  const effectiveTestDisabledReason = testDisabledReason || flowTestDisabledReason;

  const rootBaseSx: SxProps<Theme> = useMemo(() => ({ display: display ?? 'flex', flexDirection: 'column', height: '100%', minHeight: 0, flex: 1 }), [display]);
  const rootSx = combineSx(rootBaseSx, sx);

  return (
    <Box sx={rootSx}>
      <Box sx={headerContainerSx(isFromChat)}>
        <TabGroupButton
          items={[...buttonItems]}
          value={mode}
          onChange={onSelectChatMode}
        />
        <Box sx={actionButtonsContainerSx}>
          {mode === PipelineEditorMode.Flow && (
            <AddNodeMenu
              onAddNode={onAddNode}
              disabled={disabled}
            />
          )}
          {onTestPipeline && !isTestPanelOpen && (
            <TestPipelineButton
              onClick={onTestPipeline}
              disabledReason={effectiveTestDisabledReason}
            />
          )}
        </Box>
      </Box>
      <PipelineYamlSerializationAlert error={serializationError} />
      <Box sx={editorContainerSx}>
        <FlowEditorErrorBoundary display={mode}>
          <Suspense fallback={<Box sx={{ flex: 1 }}>Preparing the flow editor...</Box>}>
            <FlowWrapperLazy
              ref={flowEditorRef}
              stopRun={stopRun}
              mode={mode}
              yamlJsonObject={yamlJsonObject}
              setYamlJsonObject={setYamlJsonObject}
              initialNodes={initialNodes}
              initialEdges={initialEdges}
              layoutVersion={layoutVersion}
              resetFlag={resetFlag}
              onResetHandled={clearResetFlag}
              onLayoutVersionChange={setLayoutVersion}
              noBorder={isFromChat}
              disabled={disabled}
              versionTools={versionTools}
              llmSettings={llmSettings}
              onCanvasNodeInteraction={onCanvasNodeInteraction}
            />
          </Suspense>
        </FlowEditorErrorBoundary>
        {mode === PipelineEditorMode.Yaml && (
          <Box sx={yamlEditorContainerSx(isFromChat, isSmallWindow)}>
            <YamlCodeEditor
              ref={yamlEditorRef}
              code={yamlCode || ''}
              onChangeCode={onChangeCode}
              disabled={disabled ?? false}
            />
          </Box>
        )}
      </Box>
    </Box>
  );
});
