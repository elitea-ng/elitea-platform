import type { ReactNode } from 'react';

import type { YamlPipelineDocument } from '../flow-editor/helpers/pipelineFlow.types';

export type PickerKind = 'toolkit' | 'mcp' | 'agent' | 'pipeline';
export type ToolRefKind = 'toolkit' | 'application';

export interface PickerToolAuthor {
  readonly id?: number | string | undefined;
  readonly name?: string | undefined;
  readonly email?: string | undefined;
}

export interface PickerToolSettings {
  readonly application_id?: number | undefined;
  readonly application_version_id?: number | undefined;
  readonly selected_tools?: readonly (string | { readonly name?: string | undefined })[] | undefined;
  readonly available_tools?: readonly (string | { readonly name?: string | undefined })[] | undefined;
  readonly url?: string | undefined;
  readonly [key: string]: unknown;
}

export interface PickerToolEntry {
  readonly id?: number | string | undefined;
  readonly tool_id?: number | string | undefined;
  readonly type?: string | undefined;
  readonly name?: string | undefined;
  readonly toolkit_name?: string | undefined;
  readonly description?: string | undefined;
  readonly agent_type?: string | undefined;
  readonly online?: boolean | undefined;
  readonly icon_meta?: Readonly<Record<string, unknown>> | undefined;
  readonly meta?: {
    readonly mcp?: boolean | undefined;
    readonly [key: string]: unknown;
  } | undefined;
  readonly settings?: PickerToolSettings | undefined;
  readonly author?: PickerToolAuthor | undefined;
  readonly author_id?: number | string | undefined;
  readonly variables?: readonly { readonly name: string; readonly value: string }[] | undefined;
  readonly version_details?: {
    readonly id?: number | undefined;
    readonly agent_type?: string | undefined;
    readonly icon_meta?: Readonly<Record<string, unknown>> | undefined;
    readonly author?: PickerToolAuthor | undefined;
    readonly variables?: readonly { readonly name: string; readonly value: string }[] | undefined;
    readonly meta?: {
      readonly internal_tools?: readonly string[] | undefined;
      readonly [key: string]: unknown;
    } | undefined;
  } | undefined;
}

export interface ApplicationListRow {
  readonly id: number;
  readonly name: string;
  readonly description?: string | undefined;
  readonly agent_type?: string | undefined;
  readonly icon_meta?: Readonly<Record<string, unknown>> | undefined;
  readonly has_interrupt?: boolean | undefined;
  readonly has_swarm?: boolean | undefined;
  readonly agent_subtree_tiers?: number | undefined;
  readonly max_agent_nesting_tiers?: number | undefined;
  readonly version_details?: PickerToolEntry['version_details'];
}

export interface ToolkitListRow {
  readonly id: number | string;
  readonly name: string;
  readonly type?: string | undefined;
  readonly toolkit_name?: string | undefined;
  readonly description?: string | undefined;
  readonly online?: boolean | undefined;
  readonly icon_meta?: Readonly<Record<string, unknown>> | undefined;
  readonly meta?: { readonly mcp?: boolean | undefined; readonly [key: string]: unknown } | undefined;
  readonly settings?: PickerToolSettings | undefined;
  readonly author?: PickerToolAuthor | undefined;
}

export interface PickerItem<TSource = PickerToolEntry | ApplicationListRow | ToolkitListRow> {
  readonly key: string;
  readonly id: number | string | undefined;
  readonly name: string;
  readonly label: string;
  readonly description: string;
  readonly iconMeta?: Readonly<Record<string, unknown>> | undefined;
  readonly source: TSource;
  readonly isAttached: boolean;
  readonly inUse: boolean;
  readonly disabledReason: string;
  readonly isPipeline?: boolean | undefined;
  readonly mcp?: boolean | undefined;
}

export interface PickerGroup<TItem extends PickerItem = PickerItem> {
  readonly key: string;
  readonly title: string;
  readonly items: readonly TItem[];
}

export interface PickerCreateAction {
  readonly key: string;
  readonly label: string;
  readonly onClick: () => void;
  readonly testId?: string | undefined;
}

export interface ToolReference {
  readonly kind: ToolRefKind;
  readonly name: string;
  readonly nodeIds: string[];
  readonly toolNames: Set<string>;
}

export interface AttachmentGroupItem {
  readonly key: string;
  readonly tool: PickerToolEntry;
  readonly kind: PickerKind;
  readonly usedBy: readonly string[];
}

export interface AttachmentGroup {
  readonly kind: PickerKind;
  readonly items: readonly AttachmentGroupItem[];
}

export interface AttachmentVersionChange {
  readonly tool: PickerToolEntry;
  readonly initialTool: PickerToolEntry;
}

export interface AttachmentDiff {
  readonly added: readonly PickerToolEntry[];
  readonly removed: readonly PickerToolEntry[];
  readonly selectedToolsChanged: readonly PickerToolEntry[];
  readonly versionChanged: readonly AttachmentVersionChange[];
  readonly hasChanges: boolean;
}

export interface PickerListState<TRow> {
  readonly rows: readonly TRow[];
  readonly total: number;
  readonly isLoading: boolean;
  readonly isFetching: boolean;
  readonly isError: boolean;
  readonly hasMore: boolean;
  readonly onLoadMore: () => void;
  readonly reload: () => void;
}

export interface ToolValidationMessage {
  readonly message: string;
  readonly error_type?: string | undefined;
  readonly credential_id?: string | undefined;
  readonly model_name?: string | undefined;
  readonly refusalCode?: string | undefined;
}

export interface ToolValidationOverride {
  readonly message: string;
  readonly errorType?: string | undefined;
  readonly credentialId?: string | undefined;
  readonly modelName?: string | undefined;
  readonly refusalCode?: string | undefined;
  readonly isForeignPrivateCredential?: boolean | undefined;
}

export interface InternalModuleToolItem {
  readonly name: string;
  readonly title: string;
  readonly icon?: ReactNode | undefined;
  readonly disabled?: boolean | undefined;
  readonly unavailable?: boolean | undefined;
  readonly unavailableTooltip?: string | undefined;
  readonly infoTooltip?: string | undefined;
}

export type GetToolkitNameFn = (tool: PickerToolEntry | ToolkitListRow) => string | undefined;

export type PipelineYamlDocument = YamlPipelineDocument | null | undefined;
