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
import { useTheme, type SxProps, type Theme } from '@mui/material/styles';
import { useRouterState } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { handleCopy } from '@/shared/lib/clipboard';
import { PipelineEditorMode } from '@/shared/lib/enums';
import type { CodeMirrorEditorHandle } from '@/shared/ui/CodeMirrorEditor';
import { CopyLinkIcon } from '@/shared/ui/icons/copy-link-icon';
import { RefreshIcon } from '@/shared/ui/icons/refresh-icon';
import { TabGroupButton } from '@/shared/ui/TabGroupButton';
import type { TabGroupButtonItem } from '@/shared/ui/TabGroupButton';
import { combineSx } from '@/shared/ui/lib/combineSx';

import type { AiAssistantLlmSettings } from '../api/aiAssistantPredict';
import { useIsPipelineYamlCodeDirty, isChatPath } from '../lib/hooks/useIsPipelineYamlCodeDirty';
import { useIsSmallWindow } from '../lib/hooks/useIsSmallWindow';
import { parseYaml } from '../lib/flow-editor/helpers/parsePipeline.helpers';
import type { RunSocketEvent } from '../lib/flow-editor/helpers/parseRunsByEvent.support';
import type { FlowEdge, FlowNode, SetYamlJsonObject } from '../lib/flow-editor/reactFlowTypes';
import type { YamlPipelineDocument } from '../lib/flow-editor/helpers/pipelineFlow.types';
import { usePipelineYamlStore } from '../model/pipelineYamlStore';
import { AddNodeMenu } from './AddNodeMenu';
import type { PipelineNodeType } from '../lib/flow-editor/constants/flowEditor.constants';
import type { FlowEditorHandle } from './FlowEditor';
import type { PipelineToolEntry } from './select/pipelineToolEntry.types';
import { YamlCodeEditor } from './YamlCodeEditor';
import type { PipelineEditorModeValue } from './FlowWrapper';

const FlowWrapperLazy = lazy(() => import('./FlowWrapper').then((m) => ({ default: m.FlowWrapper })));

/**
 * Ported from `apps/elitea-ui/src/pages/Pipelines/Components/EditorPanel.jsx`.
 *
 * **Real, load-bearing redesign of the yaml<->flow-graph plumbing, matching
 * the mission's own note that this sub-unit is "the LAST thing A2 builds"
 * and now transitively depends on sibling unit A2k's landed `FlowEditor.tsx`
 * — see `FlowWrapper.tsx`'s own doc comment for the full evidence trail on
 * why `FlowEditor` needs `yamlJsonObject`/`initialNodes`/`initialEdges`/
 * `layoutVersion`/`resetFlag`/`onResetHandled`/`onLayoutVersionChange` as
 * explicit props now, where the baseline read them off Redux.** This file
 * is the composition point that owns/computes all of them:
 *  - `yamlCode`/`yamlJsonObject`/`resetFlag`/`layoutVersion` are read
 *    directly from `../model/pipelineYamlStore.ts` (A2n's own store) —
 *    NOT via props, matching `PipelineEditor.tsx`'s own doc comment
 *    confirming this is the intended shape ("the zustand equivalent...
 *    `EditorPanel` reads them directly").
 *  - `initialNodes`/`initialEdges` are freshly derived from `yamlJsonObject`
 *    via `ParsePipelineHelpers.parseYaml` (real, landed, unit A2c) — the
 *    "nodes/edges freshly parsed from the current YAML" the baseline's own
 *    `FlowEditor.jsx` computed internally from Redux state.
 *
 * **Deprecated/dropped, disclosed:**
 *  - `StyledTooltip` (`@/ComponentsLib/Tooltip.jsx`) -> plain MUI `Tooltip`
 *    — the baseline's OWN `AddNodeMenu.jsx` already used plain MUI
 *    `Tooltip` for the identical "add node" affordance; no behavioural
 *    difference beyond default MUI tooltip chrome.
 *  - `ChunkHelpers.lazyWithRetry` (not ported anywhere in this worktree,
 *    verified: `grep -rl lazyWithRetry src` — zero hits) -> plain React
 *    `lazy()`. The retry-on-chunk-load-failure behaviour is dropped, but
 *    this file's own `ErrorBoundary` fallback already offers a "Reload the
 *    page" recovery action for exactly that failure mode.
 *  - `react-error-boundary` (not a dependency of this app — verified:
 *    `grep -n react-error-boundary package.json`, zero hits) -> a small
 *    local class-component error boundary, React's own documented pattern
 *    for exactly this (no third-party API surface beyond what React itself
 *    provides).
 *  - `data-tour={PIPELINE_TOUR_TARGET_IDS.workspace}` (baseline:
 *    `features/interactive-tours`) — dropped, same treatment this batch's
 *    other pipelines files already give that out-of-scope domain.
 *  - `useToast` — no toast/snackbar primitive exists yet in this app (see
 *    `usePipelineChatSwitchVersion.ts`'s own doc comment for the established
 *    convention); the "code copied" confirmation toast is dropped, the copy
 *    action itself is real.
 */
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
            placement="top"
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
  const { setYamlDirty, stopRun, display, sx, disabled, versionTools, llmSettings } = props;
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

  const setYamlJsonObject = useCallback<SetYamlJsonObject>(
    (next, options) => editPipelineYamlDocument(next, options),
    [editPipelineYamlDocument],
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
    () => Object.entries(PipelineEditorMode).map(([label, value]) => ({ label, value })),
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
      }
      setMode(newMode as PipelineEditorModeValue);
    },
    [mode, onParseCodeToJson, storeSetYamlCode],
  );

  const onAddNode = useCallback((type: PipelineNodeType) => {
    flowEditorRef.current?.onAddNode(type);
  }, []);

  const onCopy = useCallback(() => {
    void handleCopy(yamlCode);
  }, [yamlCode]);

  const initialNodes: readonly FlowNode[] = useMemo(() => parseYaml(yamlJsonObject).nodes as unknown as readonly FlowNode[], [yamlJsonObject]);
  const initialEdges: readonly FlowEdge[] = useMemo(() => parseYaml(yamlJsonObject).edges as unknown as readonly FlowEdge[], [yamlJsonObject]);

  const theme = useTheme();
  const rootBaseSx: SxProps<Theme> = useMemo(() => ({ display: display ?? 'flex', flexDirection: 'column', height: '100%', flex: 1 }), [display]);
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
          {mode === PipelineEditorMode.Yaml && (
            <Tooltip
              title={t('features.pipelines.editorPanel.copyYaml', 'Copy yaml code to clipboard')}
              placement="top"
            >
              <IconButton onClick={onCopy}>
                <CopyLinkIcon style={{ width: '1rem', height: '1rem', fill: theme.vars.palette.icon.fill.secondary }} />
              </IconButton>
            </Tooltip>
          )}
        </Box>
      </Box>
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
