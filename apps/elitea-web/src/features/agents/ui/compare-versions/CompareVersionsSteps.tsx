import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { AgentCompareData } from '../../lib/compareVersions';
import { matchDependencies } from '../../lib/compareVersions';
import { TextDiffHighlight } from '../ai-edit/TextDiffHighlight';

/**
 * The three comparison steps, ported from `apps/elitea-ui/src/[fsd]/entities/
 * compare-versions/ui/steps/`.
 *
 * **DISCLOSED SCOPE — read-only.** Each baseline step wraps its two panes in
 * an EDITABLE `TextDiffHighlight` plus a per-field "Save <version>" button,
 * so the modal doubles as a two-sided editor writing straight back to either
 * version. That half is not ported: it needs a per-side version-update write
 * path (`onSaveLeft`/`onSaveRight`, `savingLeftKeys`/`savingRightKeys`, and
 * the discard-unsaved-changes confirmation that guards it) which no caller in
 * this app owns yet, and `../ai-edit/TextDiffHighlight.tsx`'s own doc comment
 * records why its `contentEditable` mode was not ported either. Comparing is
 * the whole of what this modal does; editing stays where the editor is.
 * TODO(#compare-versions-save): add the two save paths when a version-update
 * caller exists.
 */

/** One field's two panes, side by side. Port of `EditEntityComparisonLayout`. */
function ComparisonRow(props: {
  label: string;
  leftVersionName: string;
  rightVersionName: string;
  left: ReactNode;
  right: ReactNode;
  noDiff: boolean;
}): ReactNode {
  const { label, leftVersionName, rightVersionName, left, right, noDiff } = props;
  return (
    <Box sx={rowSx}>
      <Typography variant="labelMedium">{label}</Typography>
      {noDiff && (
        <Typography
          variant="bodySmall"
          color="text.secondary"
          sx={noDiffSx}
        >
          {t('features.agents.compareVersions.noDiff', 'No differences in this section.')}
        </Typography>
      )}
      <Box sx={columnsSx}>
        <Box sx={paneSx}>
          <Typography
            variant="labelSmall"
            color="text.secondary"
          >
            {leftVersionName}
          </Typography>
          {left}
        </Box>
        <Box sx={paneSx}>
          <Typography
            variant="labelSmall"
            color="text.secondary"
          >
            {rightVersionName}
          </Typography>
          {right}
        </Box>
      </Box>
    </Box>
  );
}

export interface CompareStepProps {
  readonly leftVersionName: string;
  readonly rightVersionName: string;
  readonly left: AgentCompareData;
  readonly right: AgentCompareData;
}

export function CompareInstructionsStep({ leftVersionName, rightVersionName, left, right }: CompareStepProps): ReactNode {
  return (
    <ComparisonRow
      label={t('features.agents.compareVersions.instructions', 'Instructions')}
      leftVersionName={leftVersionName}
      rightVersionName={rightVersionName}
      noDiff={left.instructions === right.instructions}
      left={
        <TextDiffHighlight
          original={right.instructions}
          modified={left.instructions}
          mode="modified"
        />
      }
      right={
        <TextDiffHighlight
          original={left.instructions}
          modified={right.instructions}
          mode="modified"
        />
      }
    />
  );
}

function startersEqual(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((entry, index) => entry === right[index]);
}

/** The starters of both sides, padded so row `n` on the left faces row `n` on the right. */
function StarterList(props: { own: readonly string[]; other: readonly string[]; rows: number }): ReactNode {
  const { own, other, rows } = props;
  return (
    <Box sx={startersSx}>
      {Array.from({ length: rows }, (_unused, index) => (
        <TextDiffHighlight
          key={index}
          original={other[index] ?? ''}
          modified={own[index] ?? ''}
          mode="modified"
        />
      ))}
    </Box>
  );
}

export function CompareUserInteractionStep({ leftVersionName, rightVersionName, left, right }: CompareStepProps): ReactNode {
  const rows = Math.max(left.conversationStarters.length, right.conversationStarters.length);
  return (
    <Box sx={stepSx}>
      <ComparisonRow
        label={t('features.agents.compareVersions.welcomeMessage', 'Welcome message')}
        leftVersionName={leftVersionName}
        rightVersionName={rightVersionName}
        noDiff={left.welcomeMessage === right.welcomeMessage}
        left={
          <TextDiffHighlight
            original={right.welcomeMessage}
            modified={left.welcomeMessage}
            mode="modified"
          />
        }
        right={
          <TextDiffHighlight
            original={left.welcomeMessage}
            modified={right.welcomeMessage}
            mode="modified"
          />
        }
      />
      <ComparisonRow
        label={t('features.agents.compareVersions.starters', 'Conversation starters')}
        leftVersionName={leftVersionName}
        rightVersionName={rightVersionName}
        noDiff={startersEqual(left.conversationStarters, right.conversationStarters)}
        left={
          <StarterList
            own={left.conversationStarters}
            other={right.conversationStarters}
            rows={rows}
          />
        }
        right={
          <StarterList
            own={right.conversationStarters}
            other={left.conversationStarters}
            rows={rows}
          />
        }
      />
    </Box>
  );
}

/** One matched dependency's cell — an empty slot keeps the two columns aligned. */
function DependencyCell(props: { name: string | undefined; entityType: string | undefined; unique: boolean }): ReactNode {
  const { name, entityType, unique } = props;
  if (name === undefined) return <Box sx={emptySlotSx} />;
  return (
    <Box sx={unique ? uniqueCardSx : cardSx}>
      <Typography variant="bodyMedium">{name}</Typography>
      <Typography
        variant="bodySmall"
        color="text.secondary"
      >
        {entityType}
      </Typography>
    </Box>
  );
}

export function CompareToolsSkillsStep({ leftVersionName, rightVersionName, left, right }: CompareStepProps): ReactNode {
  const rows = useMemo(() => matchDependencies(left.tools, right.tools), [left.tools, right.tools]);
  const noDiff = rows.length > 0 && rows.every((row) => row.left !== null && row.right !== null);

  if (rows.length === 0) {
    return (
      <Box sx={stepSx}>
        <Typography
          variant="bodyMedium"
          color="text.secondary"
        >
          {t('features.agents.compareVersions.noTools', 'No tools, agents, pipelines or skills attached')}
        </Typography>
      </Box>
    );
  }

  return (
    <ComparisonRow
      label={t('features.agents.compareVersions.toolsSkills', 'Tools & Skills')}
      leftVersionName={leftVersionName}
      rightVersionName={rightVersionName}
      noDiff={noDiff}
      left={
        <Box sx={startersSx}>
          {rows.map((row) => (
            <DependencyCell
              key={row.key}
              name={row.left?.name}
              entityType={row.left?.entityType}
              unique={row.right === null}
            />
          ))}
        </Box>
      }
      right={
        <Box sx={startersSx}>
          {rows.map((row) => (
            <DependencyCell
              key={row.key}
              name={row.right?.name}
              entityType={row.right?.entityType}
              unique={row.left === null}
            />
          ))}
        </Box>
      }
    />
  );
}

const stepSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.5rem' };
const rowSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const noDiffSx: SxProps<Theme> = { fontStyle: 'italic' };
const columnsSx: SxProps<Theme> = { display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '1rem' };
const startersSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const paneSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  padding: '0.5rem 1rem',
  minWidth: 0,
  overflowWrap: 'anywhere',
  borderRadius: theme.vars.shape.radiusMd,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
});
const cardSx: SxProps<Theme> = (theme) => ({
  padding: '0.5rem',
  borderRadius: theme.vars.shape.radiusSm,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
});
const uniqueCardSx: SxProps<Theme> = (theme) => ({
  padding: '0.5rem',
  borderRadius: theme.vars.shape.radiusSm,
  border: `0.0625rem solid ${theme.vars.palette.warning.main}`,
});
const emptySlotSx: SxProps<Theme> = { minHeight: '3rem' };
