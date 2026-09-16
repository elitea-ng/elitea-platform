/**
 * The chat page's entity-editor column (#940 A12).
 *
 * Extracted from `ChatWithEditors.tsx` for its §3.5 400-line file budget and
 * that component's own cyclomatic budget — three editors, each with its own
 * visibility flag and its own `deps` object, is most of both.
 *
 * WHAT THE COLUMN IS FOR, and why it is not a fragment. Before this, the three
 * editors were siblings of the chat ROW inside the same fragment, so each
 * rendered its own `height: 100%` block BELOW the chat inside a `main` that is
 * `display: block; height: 100vh` with no overflow of its own
 * (`widgets/app-shell/ui/AppShell.tsx`). `EditorShell` is written as a fixed
 * header (Close, Discard, Save) over one `overflow: auto` body, and that layout
 * only works inside a parent with a bounded height: given none, the header sat
 * a viewport below the fold and the whole document scrolled instead of the
 * form. Those are ELITEA-0918/0919/0920/0922/0928 exactly. Nothing in
 * `EditorShell` needed changing — it needed a parent with a height.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';

import { AgentEditor } from '@/features/agents';
import { PipelineEditor } from '@/features/pipelines';
import { ToolkitEditor } from '@/features/toolkits';

import { toCreatedResult } from './ChatWithEditors.helpers';
import { renderAgentEditorShell, renderPipelineEditorShell, renderToolkitEditorShell } from './EditorShell';
import { renderPipelineCreateForm } from './PipelineCreateForm';
import type { UseChatPipelineConfigResult } from './useChatPipelineConfig';

/**
 * Each editor's slice of `useChatWithEditors`' result, grouped per entity.
 *
 * Typed loosely on purpose: every member below is already the exact return
 * value of a hook this file does not own, and restating those shapes here
 * would be a second declaration to keep in step with the first. The grouping
 * is what matters — it keeps this component's prop count at three.
 */
interface AgentEditorGroup {
  readonly isEditing: boolean;
  readonly agentForEditor: Parameters<typeof AgentEditor>[0]['agent'];
  readonly editAgent: { readonly isCreateMode: boolean; readonly onCloseAgentEditor: () => void };
  readonly agentCreation: { readonly onAgentCreated: (result: ReturnType<typeof toCreatedResult>) => unknown };
}

interface PipelineEditorGroup {
  readonly isEditing: boolean;
  readonly editPipeline: {
    readonly editingPipeline: Parameters<typeof PipelineEditor>[0]['pipeline'];
    readonly isPipelineCreateMode: boolean;
    readonly onClosePipelineEditor: () => void;
  };
  readonly pipelineCreation: { readonly onPipelineCreated: (result: ReturnType<typeof toCreatedResult>) => unknown };
  readonly config: UseChatPipelineConfigResult;
}

interface ToolkitEditorGroup {
  readonly isEditing: boolean;
  readonly editToolkit: {
    readonly editingToolkit: Parameters<typeof ToolkitEditor>[0]['toolkit'];
    readonly onCloseToolkitEditor: () => void;
  };
  readonly toolkitCreation: { readonly onToolkitCreated: (result: Parameters<NonNullable<Parameters<typeof ToolkitEditor>[0]['onToolkitCreated']>>[0]) => unknown };
  readonly toolkitWriteDeps: {
    readonly createToolkit: Parameters<typeof ToolkitEditor>[0]['deps']['createToolkit'];
    readonly saveToolkit: Parameters<typeof ToolkitEditor>[0]['deps']['saveToolkit'];
  };
}

export interface ChatEditorColumnProps {
  readonly agent: AgentEditorGroup;
  readonly pipeline: PipelineEditorGroup;
  readonly toolkit: ToolkitEditorGroup;
}

export function ChatEditorColumn({ agent, pipeline, toolkit }: ChatEditorColumnProps): ReactNode {
  return (
    <Box
      data-testid="chat-editor-panel"
      sx={{
        flex: '0 0 auto',
        width: { xs: '100%', md: '40rem' },
        maxWidth: '100%',
        height: '100%',
        minHeight: 0,
        mx: 2,
        display: 'flex',
        flexDirection: 'column',
        boxSizing: 'border-box',
      }}
    >
      {agent.isEditing && (
        <AgentEditor
          agent={agent.agentForEditor}
          isVisible={agent.isEditing}
          isCreateMode={agent.editAgent.isCreateMode}
          onCloseAgentEditor={agent.editAgent.onCloseAgentEditor}
          onAgentCreated={(result) => void agent.agentCreation.onAgentCreated(toCreatedResult(result))}
          deps={{ renderShell: renderAgentEditorShell }}
        />
      )}

      {pipeline.isEditing && (
        <PipelineEditor
          pipeline={pipeline.editPipeline.editingPipeline}
          isVisible={pipeline.isEditing}
          isCreateMode={pipeline.editPipeline.isPipelineCreateMode}
          onClosePipelineEditor={pipeline.editPipeline.onClosePipelineEditor}
          onPipelineCreated={(result) => pipeline.pipelineCreation.onPipelineCreated(toCreatedResult(result))}
          deps={{
            renderShell: renderPipelineEditorShell,
            /* Create mode's form body — the slot that was empty (see `./PipelineCreateForm.tsx`). */
            renderCreateForm: renderPipelineCreateForm,
            /* Edit mode's form body and its Save — one hook, one draft (see `./useChatPipelineConfig.tsx`). */
            renderConfigurationPanels: pipeline.config.renderConfigurationPanels,
            ...(pipeline.config.onSaveVersion !== undefined ? { onSaveVersion: pipeline.config.onSaveVersion } : {}),
            isSavingVersion: pipeline.config.isSavingVersion,
          }}
        />
      )}

      {toolkit.isEditing && (
        <ToolkitEditor
          toolkit={toolkit.editToolkit.editingToolkit}
          isVisible={toolkit.isEditing}
          onCloseToolkitEditor={toolkit.editToolkit.onCloseToolkitEditor}
          onToolkitCreated={(result) => void toolkit.toolkitCreation.onToolkitCreated(result)}
          deps={{
            renderShell: renderToolkitEditorShell,
            createToolkit: toolkit.toolkitWriteDeps.createToolkit,
            saveToolkit: toolkit.toolkitWriteDeps.saveToolkit,
          }}
        />
      )}
    </Box>
  );
}
