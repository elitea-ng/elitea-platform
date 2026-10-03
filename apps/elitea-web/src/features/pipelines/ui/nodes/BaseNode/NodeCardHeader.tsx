/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/BaseNode/NodeCardHeader.jsx` (469 lines, unit A2e). The rename logic
 * (baseline lines 63-189) lives in `./NodeCardHeader.rename.ts` and the
 * style factory (baseline lines 361-467) in `./NodeCardHeader.styles.ts` --
 * both purely to keep this file under the §3.5 400-line budget and every
 * function under the 12-branch complexity ceiling (`max-lines`/
 * `complexity`, enforced live by oxlint).
 *
 * DISCLOSED REDESIGNS, each forced by a real, verified constraint (matching
 * this batch's established "ambient context -> parameter" convention, see
 * `lib/flow-editor/hooks/useNodeAiAssistantConfig.ts`'s own header):
 *
 *  1. `useFormikContext()` (baseline lines 39-40, reading
 *     `values.version_details.tools` to build the duplicate-toolkit-name
 *     check) -- this app has no Formik (react-hook-form + zod). Replaced
 *     with a plain `toolNames?: readonly string[]` prop; the caller (a
 *     react-hook-form-backed pipeline-editor form, out of this pure-
 *     scaffolding sub-unit's scope) reads its own `version_details.tools`
 *     field (e.g. via `useWatch`) and derives the name list the same way
 *     the baseline's `useMemo` did (`tool.toolkit_name || tool.name`).
 *
 *  2. `useToast().toastError(...)` (baseline lines 21, 75-79) -- no toast
 *     primitive exists yet anywhere in this app (`shared/ui`'s own
 *     `ControlsDropdown` etc. do not have one; same documented gap as
 *     `features/agents/ui/EnhancedCardToolActions.tsx`'s own "DEVIATION
 *     (toast)"). Replaced with `onDuplicateName?: (message: string) =>
 *     void`, called with the exact same two message strings the baseline
 *     passed to `toastError`.
 *
 *  3. `DotMenu` (baseline's `@/components/DotMenu`) -> `shared/ui/
 *     ControlsDropdown` (this app's promoted equivalent, a from-scratch
 *     re-derivation per that component's own doc comment, NOT a DotMenu
 *     wrapper). Its API is `items: ControlsDropdownItem[]` (each needing a
 *     `key`) instead of DotMenu's untyped `children` array, and it owns its
 *     own anchor/transform origins internally -- the baseline's explicit
 *     `anchorOrigin`/`transformOrigin`/`slotProps` props have no equivalent
 *     to forward and are dropped.
 *
 *  4. `IconButton` `variant="elitea"` (baseline lines 318-319) -- this app's
 *     `IconButton` has no typed `variant` prop; ITS single skin applies
 *     unconditionally via `styleOverrides.root`, gated only by `color`
 *     (`shared/brand/mui-overrides/MuiIconButton.ts`'s own doc comment).
 *     Dropped; `color="tertiary"` alone reproduces the baseline's look.
 *
 *  5. `DeleteIcon` (baseline: `@/components/Icons/DeleteIcon`, a custom SVG)
 *     is not part of S2's ported `shared/ui/icons/` set (no `delete-
 *     icon.tsx` -- verified by directory listing). `DeleteOutlined` from
 *     `@mui/icons-material` is used as the interim substitute, same class
 *     of gap `shared/ui/ControlsDropdown`'s own `MoreVertIcon` TODO
 *     documents.
 *
 *  6. `autoFocus` on the rename `TextField` (baseline line 293) is dropped
 *     -- this app's `jsx-a11y/no-autofocus` gate bans it outright (a11y:
 *     autofocus disorients screen-reader/keyboard users, same rule already
 *     enforced repo-wide, no per-file exemption available). The double-
 *     click-to-rename affordance still works; the caret simply is not
 *     auto-placed.
 *
 * `NodeHelpers`/`DeprecatedConstants` are this unit's own sibling files
 * (`../../../lib/flow-editor/helpers/node.helpers`,
 * `../../../lib/flow-editor/constants/deprecated.constants`) -- intra-slice
 * imports, no cross-slice boundary crossed (R-L3).
 */
import type { ChangeEvent, ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import TextField from '@mui/material/TextField';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import { useTheme } from '@mui/material/styles';

import { PipelineNodeTypes } from '../../../lib/flow-editor/constants/flowEditor.constants';
import { DeprecatedTips } from '../../../lib/flow-editor/constants/deprecated.constants';
import { getNodeIconByType, isDeprecatedNodeType } from '../../../lib/flow-editor/helpers/node.helpers';
import { isCompilerLegalNodeId } from '../../../lib/flow-editor/helpers/flowEditor.helpers';
import type { FlowEdge, FlowNode, SetFlowEdges, SetFlowNodes } from '../../../lib/flow-editor/reactFlowTypes';
import type { YamlPipelineDocument } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import { renameFlowEdge, renameFlowNode, renameYamlDocument } from './NodeCardHeader.rename';
import { nodeCardHeaderStyles } from './NodeCardHeader.styles';
import { ControlsDropdown, type ControlsDropdownItem } from '@/shared/ui/ControlsDropdown';
import { TextWithLink } from '@/shared/ui/TextWithLink';
import { AttentionIcon } from '@/shared/ui/icons/attention-icon';
import { CollapseIcon } from '@/shared/ui/icons/collapse-icon';
import { EntrypointIcon } from '@/shared/ui/icons/entrypoint-icon';
import { ExpandIcon } from '@/shared/ui/icons/expand-icon';
import { t } from '@/shared/i18n';

export interface NodeCardHeaderProps {
  readonly name: string;
  readonly isEntrypoint: boolean;
  readonly isExpanded: boolean;
  readonly onExpand: (next: boolean) => void;
  /** Forwarded by `NodeCard` (baseline parity) -- never read here, same as the baseline's own unused destructure. */
  readonly isConditionNode?: boolean;
  readonly type: string;
  readonly id: string;
  readonly disabled?: boolean;
  readonly yamlJsonObject: YamlPipelineDocument;
  readonly setYamlJsonObject: (next: YamlPipelineDocument) => void;
  readonly setFlowNodes: SetFlowNodes;
  readonly setFlowEdges: SetFlowEdges;
  readonly isRunningPipeline?: boolean;
  readonly handleDeleteNode?: (id: string) => void;
  /** Replaces the baseline's ambient `useFormikContext()` tools read -- see module doc comment, deviation 1. */
  readonly toolNames?: readonly string[];
  /** Replaces the baseline's `toastError(...)` call -- see module doc comment, deviation 2. */
  readonly onDuplicateName?: (message: string) => void;
}

/**
 * Why a rename must be refused, or `undefined` to let it through.
 *
 * The FIRST branch is not a baseline behaviour: a node id is a runtime graph
 * identifier, and `valid_graph_id`
 * (`services/elitea-worker-rust/src/agents/graph/yaml.rs:362`) admits ASCII
 * alphanumerics plus `_ - . :` and nothing else. Renaming a node to
 * `"My Agent"` used to be accepted here and then rewritten into
 * `entry_point`, every `transition` and every route target by
 * `renameYamlDocument` — producing a document the compiler refuses whole
 * (`graph.pipeline.invalid_configuration`). Minting legal ids
 * (`getInitialNodeId`) is pointless if the rename box can undo it.
 */
function findRenameRejection(
  inputtedName: string,
  name: string,
  yamlJsonObject: YamlPipelineDocument,
  toolNames: readonly string[],
): string | undefined {
  if (!isCompilerLegalNodeId(inputtedName)) {
    // Literal, not `ValidationErrors.NodeNameInvalid`: `scripts/
    // i18n-backfill.mjs` extracts `t(key, fallback)` pairs statically and
    // cannot resolve a fallback that is an identifier, so a constant here
    // fails the en.json sync gate as UNRESOLVED. The two duplicate-name
    // messages below inline their copy for the same reason.
    return t(
      'pipelines.flowEditor.nodeCardHeader.invalidNodeName',
      'Only letters, numbers and _ - . : are allowed — no spaces.',
    );
  }

  const foundNodeName = yamlJsonObject.nodes?.find(
    node => node.id !== name && node.id.replace(/\s/g, '') === inputtedName.replace(/\s/g, ''),
  );
  if (foundNodeName) {
    return t(
      'pipelines.flowEditor.nodeCardHeader.duplicateNodeName',
      'The name has been used by other nodes, please input a new name!',
    );
  }

  const foundToolName = toolNames.find(toolName => toolName.replace(/\s/g, '') === inputtedName.replace(/\s/g, ''));
  if (foundToolName) {
    return t(
      'pipelines.flowEditor.nodeCardHeader.duplicateToolName',
      `The name conflicts with an existing toolkit name "${foundToolName}", please input a new name!`,
      { toolName: foundToolName },
    );
  }

  return undefined;
}

export function NodeCardHeader(props: NodeCardHeaderProps): ReactNode {
  const {
    name,
    isEntrypoint,
    isExpanded,
    onExpand,
    type,
    id,
    disabled,
    yamlJsonObject,
    setYamlJsonObject,
    setFlowNodes,
    setFlowEdges,
    isRunningPipeline,
    handleDeleteNode,
    toolNames = [],
    onDuplicateName,
  } = props;

  const theme = useTheme();
  const [isEditingName, setIsEditingName] = useState(false);
  const [inputtedName, setInputtedName] = useState(name);

  const onDoubleClickName = useCallback(() => {
    if (type !== PipelineNodeTypes.Condition && !isRunningPipeline) {
      setIsEditingName(true);
    }
  }, [isRunningPipeline, type]);

  const handleMakeEntrypoint = useCallback(() => {
    setYamlJsonObject({ ...yamlJsonObject, entry_point: name });
  }, [name, setYamlJsonObject, yamlJsonObject]);

  const handleDelete = useCallback(() => {
    handleDeleteNode?.(id);
  }, [handleDeleteNode, id]);

  const onChange = useCallback((event: ChangeEvent<HTMLInputElement>) => {
    setInputtedName(event.target.value);
  }, []);

  const onBlur = useCallback(() => {
    if (inputtedName === name) {
      setIsEditingName(false);
      return;
    }

    // `onDuplicateName` keeps its baseline name (it is a caller-owned prop)
    // but is now the general "rename refused, here is why" channel — it also
    // carries the compiler-legality refusal added in `findRenameRejection`.
    const rejection = findRenameRejection(inputtedName, name, yamlJsonObject, toolNames);
    if (rejection) {
      onDuplicateName?.(rejection);
      setInputtedName(name);
      setIsEditingName(false);
      return;
    }

    setYamlJsonObject(renameYamlDocument(yamlJsonObject, name, inputtedName));
    // Every node runs through `renameFlowNode` -- not just the one whose id
    // matches `name` -- so a Decision/Condition node referencing the
    // renamed node by name gets its reference rewritten too (baseline:
    // `NodeCardHeader.jsx:131-179`'s single unconditional `.map`; see
    // `NodeCardHeader.rename.ts`'s own header).
    setFlowNodes((prevNodes: FlowNode[]) => prevNodes.map(node => renameFlowNode(node, name, inputtedName)));
    setFlowEdges((prevEdges: FlowEdge[]) => prevEdges.map(edge => renameFlowEdge(edge, name, inputtedName)));
    setIsEditingName(false);
  }, [inputtedName, name, onDuplicateName, setFlowEdges, setFlowNodes, setYamlJsonObject, toolNames, yamlJsonObject]);

  const styles = nodeCardHeaderStyles();

  const menuItems = useMemo((): ControlsDropdownItem[] => {
    const deleteItem: ControlsDropdownItem = {
      key: 'delete',
      label: t('pipelines.flowEditor.nodeCardHeader.delete', 'Delete'),
      icon: (
        <Box sx={styles.menuIconWrapper}>
          <DeleteOutlineIcon sx={styles.menuIconStyle} />
        </Box>
      ),
      disabled: Boolean(disabled),
      onClick: handleDelete,
    };

    if (isEntrypoint || type === PipelineNodeTypes.Condition || type === PipelineNodeTypes.Decision) {
      return [deleteItem];
    }

    return [
      {
        key: 'make-entrypoint',
        label: t('pipelines.flowEditor.nodeCardHeader.makeEntrypoint', 'Make entrypoint'),
        icon: (
          <Box sx={styles.menuIconWrapper}>
            <EntrypointIcon style={styles.menuIconStyle} />
          </Box>
        ),
        disabled: Boolean(disabled),
        onClick: handleMakeEntrypoint,
      },
      deleteItem,
    ];
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `styles` is recreated every render, deliberately excluded (matches the baseline's own dependency array, `NodeCardHeader.jsx:254-262`).
  }, [isEntrypoint, type, handleMakeEntrypoint, disabled, handleDelete]);

  const onClick = useCallback(() => {
    if (!isRunningPipeline) {
      onExpand(!isExpanded);
    }
  }, [isExpanded, onExpand, isRunningPipeline]);

  const deprecatedTip = isDeprecatedNodeType(type) ? DeprecatedTips[type] : undefined;

  return (
    <Box sx={styles.container}>
      <Box sx={styles.leftSection}>
        {isEntrypoint && (
          <Box sx={styles.entryBox}>
            <EntrypointIcon style={styles.entrypointIconStyle} />
          </Box>
        )}
        <Box sx={styles.iconWrapper}>{getNodeIconByType(type, theme)}</Box>
        {!isEditingName ? (
          <Typography
            sx={styles.nameText}
            variant="labelMedium"
            color="text.secondary"
            onDoubleClick={onDoubleClickName}
          >
            {inputtedName}
          </Typography>
        ) : (
          <Box sx={styles.inputWrapper}>
            <TextField
              value={inputtedName}
              fullWidth
              variant="standard"
              label=""
              onChange={onChange}
              onBlur={onBlur}
              className="nopan nodrag"
            />
          </Box>
        )}
      </Box>
      <Box sx={styles.rightSection}>
        {deprecatedTip && (
          <Tooltip
            title={<TextWithLink {...deprecatedTip} />}
            placement="top"
          >
            <Box sx={styles.attentionIconWrapper}>
              <AttentionIcon />
              <Typography variant="bodySmall">{t('pipelines.flowEditor.nodeCardHeader.deprecated', 'Deprecated!')}</Typography>
            </Box>
          </Tooltip>
        )}
        <IconButton
          color="tertiary"
          sx={styles.expandButton}
          onClick={onClick}
        >
          {isExpanded ? (
            <CollapseIcon style={styles.collapseIconStyle} />
          ) : (
            <ExpandIcon style={styles.expandIconStyle} />
          )}
        </IconButton>
        {isExpanded && (
          // No top-level `disabled` here (baseline: `NodeCardHeader.jsx`'s
          // `DotMenu` is never itself disabled) -- disabling the trigger
          // would block the whole menu from opening. Each `menuItems` entry
          // already carries its own `disabled: Boolean(disabled)`, matching
          // baseline's own per-item `disabled` on every menu row; the user
          // can still open the menu, just not click a disabled row.
          <ControlsDropdown
            id="node-menu"
            items={menuItems}
            triggerAriaLabel={t('pipelines.flowEditor.nodeCardHeader.nodeActions', 'Node actions')}
          />
        )}
      </Box>
    </Box>
  );
}
