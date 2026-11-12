/**
 * The EDIT-mode configuration form for an agent opened from a chat
 * conversation's participant rail (#1972).
 *
 * `AgentEditor` asks for its edit-mode body through `deps.renderConfigurationForm`
 * and its Save through `deps.onSaveVersion`; the chat composition root passed
 * neither, so opening an existing agent showed a header (name, Discard, Save)
 * over an EMPTY body with a permanently disabled Save. It is the same gap
 * `useChatPipelineConfig` closed for pipelines, and it is closed the same way:
 * this hook is that one run with `entityType: 'application'`, so there is one
 * draft, one seed and one version write for both rather than two copies.
 */
import type { ReactNode } from 'react';
import { useCallback } from 'react';

import { useChatPipelineConfig, type UseChatPipelineConfigParams } from './useChatPipelineConfig';

export interface UseChatAgentConfigResult {
  /** Passed straight to `AgentEditorDeps.renderConfigurationForm`. */
  readonly renderConfigurationForm: () => ReactNode;
  /** Passed straight to `AgentEditorDeps.onSaveVersion`; `undefined` until the stored version is resolvable. */
  readonly onSaveVersion: (() => void) | undefined;
  readonly isSavingVersion: boolean;
}

export function useChatAgentConfig(params: Omit<UseChatPipelineConfigParams, 'entityType'>): UseChatAgentConfigResult {
  const config = useChatPipelineConfig({ ...params, entityType: 'application' });
  const { renderConfigurationPanels, onSaveVersion } = config;
  const renderConfigurationForm = useCallback(
    () => (renderConfigurationPanels() as { readonly information?: ReactNode }).information ?? null,
    [renderConfigurationPanels],
  );
  const save = useCallback(() => onSaveVersion?.(() => undefined), [onSaveVersion]);
  return {
    renderConfigurationForm,
    onSaveVersion: onSaveVersion === undefined ? undefined : save,
    isSavingVersion: config.isSavingVersion,
  };
}
