import type { ReactNode } from 'react';
import { useContext } from 'react';
import Button from '@mui/material/Button';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';
import { t } from '@/shared/i18n';
import { FlowEditorContext } from '../../../lib/flow-editor/flowEditorContext';
import { extensionRecord, extensionStrings, extensionText, extensionValues, patchGraphExtensionNode } from '../../../lib/graphExtensions.helpers';
import { moveParallelBranch, nextParallelBranchId, parallelAgentReferenceCounts, parallelOwnedAgent, patchParallelBranch } from '../../../lib/graphParallel.helpers';
import { ExtensionChoice, ExtensionField } from '../graph-extensions/ExtensionFields';

interface ParallelBranchesProps {
  readonly ownerId: string;
  readonly branches: unknown;
  readonly disabled: boolean;
  readonly change: (branches: readonly unknown[]) => void;
}
function OwnedAgentSummary({ ownerId, ordinal, disabled }: { readonly ownerId: string; readonly ordinal: number; readonly disabled: boolean }): ReactNode {
  const context = useContext(FlowEditorContext);
  if (!context) return null;
  const agent = parallelOwnedAgent(context.yamlJsonObject, ownerId, ordinal);
  if (!agent) return <Typography variant="bodySmall">{t('pipelines.parallel.exactOwner', 'Select an Agent with one exact fixed branch owner.')}</Typography>;
  const outputs = extensionStrings(agent.output).join(', ');
  const task = extensionRecord(extensionRecord(agent.input_mapping)['task']);
  const removeTransition = (): void => {
    if (disabled) return;
    context.setYamlJsonObject(patchGraphExtensionNode(context.yamlJsonObject, agent.id, 'transition', undefined, true));
  };
  return <Stack spacing={1}>
    <Typography variant="bodySmall">{t('pipelines.parallel.agentScope', 'Agent {{node}} uses participant {{tool}}. Edit its own input mapping on the Agent card.', { node: agent.id, tool: extensionText(agent['tool']) })}</Typography>
    <Typography variant="bodySmall">{t('pipelines.parallel.agentOutputs', 'Declared outputs: {{outputs}}. Task mapping: {{mode}}.', { outputs, mode: extensionText(task['type']) })}</Typography>
    {agent.transition !== undefined && agent.transition !== null && <Button disabled={disabled} onClick={removeTransition}>
      {t('pipelines.parallel.removeTransition', 'Remove transition from {{node}}', { node: agent.id })}
    </Button>}
  </Stack>;
}

export function ParallelBranches({ ownerId, branches, disabled, change }: ParallelBranchesProps): ReactNode {
  const context = useContext(FlowEditorContext);
  const rows = extensionValues(branches);
  const document = context?.yamlJsonObject ?? {};
  const counts = parallelAgentReferenceCounts(document);
  const choices = (document.nodes ?? []).filter((node) => node.type === 'agent' && node.id !== ownerId
    && !counts.has(node.id) && !document.nodes?.some((entry) => entry.type === 'map' && entry['worker'] === node.id)).map((node) => node.id);
  const update = (value: readonly unknown[]): void => { if (!disabled) change(value); };
  return <Stack spacing={2}>
    {rows.map((value, ordinal) => {
      const row = extensionRecord(value);
      const number = String(ordinal + 1);
      const selected = extensionText(row['node']);
      return <Stack spacing={1} key={ordinal}>
        <ExtensionField label={t('pipelines.parallel.branchId', 'Branch {{number}} result key', { number })}
          value={extensionText(row['id'])} disabled={disabled}
          change={(next) => update(patchParallelBranch(branches, ordinal, 'id', next))} />
        <ExtensionChoice label={t('pipelines.parallel.branchAgent', 'Branch {{number}} Agent node', { number })}
          value={selected} choices={['', ...choices, ...(selected && !choices.includes(selected) ? [selected] : [])]} disabled={disabled}
          change={(next) => update(patchParallelBranch(branches, ordinal, 'node', next))} />
        <OwnedAgentSummary ownerId={ownerId} ordinal={ordinal} disabled={disabled} />
        <Stack direction="row" spacing={1}>
          <Button disabled={disabled || ordinal === 0} onClick={() => update(moveParallelBranch(branches, ordinal, -1))}>
            {t('pipelines.parallel.moveUp', 'Move branch {{number}} up', { number })}
          </Button>
          <Button disabled={disabled || ordinal === rows.length - 1} onClick={() => update(moveParallelBranch(branches, ordinal, 1))}>
            {t('pipelines.parallel.moveDown', 'Move branch {{number}} down', { number })}
          </Button>
          <Button disabled={disabled || rows.length <= 2} onClick={() => update(rows.filter((_, index) => index !== ordinal))}>
            {t('pipelines.parallel.removeBranch', 'Remove branch {{number}}', { number })}
          </Button>
        </Stack>
      </Stack>;
    })}
    <Button disabled={disabled || rows.length >= 16} onClick={() => update([...rows, { id: nextParallelBranchId(branches), node: '' }])}>
      {t('pipelines.parallel.addBranch', 'Add fixed branch')}
    </Button>
  </Stack>;
}
