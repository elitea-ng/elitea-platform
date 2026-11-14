/**
 * The change-request dialog, supplied by the page that mounts the Evaluation tab.
 *
 * Enhance with AI saves an accepted instruction patch as a draft version and then
 * offers "Request publish": the ADR-0040 change request, which lives in
 * `features/agent-lifecycle`. A feature may not import another feature
 * (`no-sideways-features`), so the page composes the two: it passes a render
 * function down through `EvaluationPanel`, and the wizard calls it. Without one,
 * the wizard offers no publish step at all — it never publishes on its own.
 */
import { createContext, useContext, type ReactNode } from 'react';

export interface RequestPublishArgs {
  readonly applicationId: number | undefined;
  readonly versionId: number;
  readonly onClose: () => void;
}

export type RenderRequestPublish = (args: RequestPublishArgs) => ReactNode;

export const RequestPublishSlot = createContext<RenderRequestPublish | undefined>(undefined);

export function useRequestPublishSlot(): RenderRequestPublish | undefined {
  return useContext(RequestPublishSlot);
}
