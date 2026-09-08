import type { Component, ErrorInfo, ReactNode } from 'react';
import { PureComponent } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ConfigurationTabProps } from '@/features/pipelines';
import { t } from '@/shared/i18n';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';

import {
  EditPipelineConfigurationPanel,
  type EditPipelineConfigurationPanelProps,
} from '../ui/EditPipelineConfigurationPanel';
import { PipelineTestChat, type PipelineTestChatProps } from '../ui/PipelineTestChat';

/** What `buildPipelineConfigurationTabSlots` needs to build the chat slot — grouped into one object so the factory keeps two parameters. */
export interface PipelineChatSlotContext {
  readonly identity: PipelineTestChatProps['identity'];
  readonly user: PipelineTestChatProps['user'];
}

/**
 * `ConfigurationTab`'s two REQUIRED slots (`renderConfigurationForm`/
 * `renderChat`), built for `EditPipeline.tsx`. **Both are real now.**
 *
 * This module was called `pipelineConfigurationTabGaps.tsx` and its last
 * remaining disclosure — that `renderConfigurationForm` could only be a
 * notice, because the `features/agents`-owned form panels were "NOT exported
 * from `features/agents/index.ts` (verified: `grep -n "^export" ...`, no such
 * names)" — had gone stale. `CreateAgentForm`, `AgentTagEditor` and
 * `AgentToolsPanel` all reached that curated API when the AGENT editor
 * mounted them, and `pages/agents/ui/EditApplicationConfigurationPanel.tsx`
 * had been composing the identical set for some time. A pipeline IS an
 * application row, so the same panels compose for it: see
 * `../ui/EditPipelineConfigurationPanel.tsx`, which the page now passes in
 * here.
 *
 * The file keeps its two other jobs — assembling the slot object and holding
 * the editor's error boundary — and no longer claims anything is missing.
 */

/**
 * `ConfigurationTab`'s required `slots` prop.
 *
 * A factory rather than a module-scope constant because both slots carry
 * page-owned content: the configuration form (whose every value belongs to
 * `EditPipeline`'s own form/version state) and the test chat (which needs the
 * pipeline's identity and the signed-in user).
 *
 * The panel's PROPS cross this boundary rather than a built element, so the
 * page has one object literal to write instead of a JSX block plus the
 * `useMemo` that a nine-input dependency array would need — and
 * `GeneralFormPanel`/`ChatPanel` call their render props straight through,
 * holding them in no dependency array, so a fresh object each render costs
 * nothing.
 */
export function buildPipelineConfigurationTabSlots(
  panel: EditPipelineConfigurationPanelProps,
  chat: PipelineChatSlotContext,
): ConfigurationTabProps['slots'] {
  return {
    renderConfigurationForm: () => <EditPipelineConfigurationPanel {...panel} />,
    renderChat: ({ settings, disableChat, ref }) => (
      <PipelineTestChat
        settings={settings}
        disableChat={disableChat}
        slotRef={ref}
        identity={chat.identity}
        user={chat.user}
      />
    ),
  };
}

interface PipelineConfigurationTabBoundaryProps {
  readonly children: ReactNode;
}
interface PipelineConfigurationTabBoundaryState {
  readonly hasError: boolean;
}

const boundaryFallbackSx: SxProps<Theme> = { padding: '1.5rem' };

/**
 * Contains a crash inside the editor subtree so it cannot take the whole
 * page (name, Save/Cancel bar, not-found handling) down with it — same
 * `PureComponent`/`getDerivedStateFromError` shape
 * `features/pipelines/ui/EditorPanel.tsx`'s own `FlowEditorErrorBoundary`
 * establishes for this "no `react-error-boundary` dependency" codebase.
 *
 * **CORRECTION — the reason this boundary used to give was FALSE, and is
 * deleted rather than reworded.** It said: "verified nobody provides a
 * `SocketClientContext.Provider` anywhere in this worktree's real (non-test)
 * render tree — zero hits". `src/app/providers/AppProviders.tsx` mounts one
 * around every page (`<SocketClientContext.Provider value={socketClient}>`),
 * and has for as long as this app has booted; the grep that "verified" the
 * claim is the kind that reads absence as proof. `useSocketClient()`
 * therefore does NOT throw here in production, and the fallback below is not
 * the expected steady state of this page — it is an error path.
 *
 * The boundary is kept anyway, for what it actually does: the flow editor is
 * the largest render subtree in the app (`@xyflow/react` plus every node
 * card), so a throw anywhere in it would otherwise unmount the page and lose
 * the user's unsaved graph along with the Save button they would use to keep
 * it. Its fallback copy is worded as a failure, not as a disclosed gap.
 */
export class PipelineConfigurationTabBoundary
  extends PureComponent<PipelineConfigurationTabBoundaryProps, PipelineConfigurationTabBoundaryState>
  implements Component
{
  override state: PipelineConfigurationTabBoundaryState = { hasError: false };

  static getDerivedStateFromError(): PipelineConfigurationTabBoundaryState {
    return { hasError: true };
  }

  override componentDidCatch(_error: Error, _info: ErrorInfo): void {
    // Intentionally silent beyond the fallback UI below — no error-reporting sink exists in this app yet (same as `FlowEditorErrorBoundary`).
  }

  override render(): ReactNode {
    if (this.state.hasError) {
      return (
        <Box
          data-testid="edit-pipeline-configuration-tab-error"
          sx={boundaryFallbackSx}
        >
          <NoResultsMessage
            title={t('pages.pipelines.editPipeline.configurationTabError.title', 'The pipeline editor could not be displayed.')}
            description={t(
              'pages.pipelines.editPipeline.configurationTabError.description',
              'Something went wrong while rendering the editor. Reload the page to try again.',
            )}
          />
        </Box>
      );
    }
    return this.props.children;
  }
}
