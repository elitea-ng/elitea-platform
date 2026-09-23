import { ToolActionStatus } from '@/shared/lib/chat';
import { t } from '@/shared/i18n';
import type { SubAgentGroupable } from '@/entities/message/lib/subAgentGrouping';

/**
 * One entry in a message's tool timeline. Left open deliberately: the baseline
 * writes provider- and toolkit-specific members onto these objects and the
 * rendering layer reads them by name, so narrowing the shape here would drop
 * data the UI still needs.
 *
 * It lives in its own leaf module because three files need the type from
 * three directions: `chatStreamShared` (helpers over it), `chatStreamReasoning`
 * (synthesises a reasoning row as one), and `convertMessagesToChatHistory`
 * (persisted timelines) — and the last two import each other's FUNCTIONS, so
 * housing the type in either of them (or in `chatStreamShared`, which needs
 * `ChatMessage` back from the converter) closed an import cycle the layer
 * gate refuses.
 */
export interface ToolAction extends SubAgentGroupable {
  readonly id: string;
  readonly status: string;
  readonly toolMeta?: Record<string, unknown> | undefined;
  readonly [key: string]: unknown;
}

/** Ending a run cannot leave a model-local compaction notice spinning. */
export function settleContextProgress(actions: readonly SubAgentGroupable[] | undefined): readonly SubAgentGroupable[] | undefined {
  if (!actions?.some((action) => (action as ToolAction).contextProgress === true && (action as ToolAction).status === ToolActionStatus.processing)) return actions;
  return actions.map((action) => {
    const progress = action as ToolAction;
    return progress.contextProgress === true && progress.status === ToolActionStatus.processing
      ? { ...progress, status: ToolActionStatus.cancelled, message: undefined, content: t('chatMessages.context.stopped', 'Compaction stopped before completion.') }
      : action;
  });
}
