import type { ReactNode } from 'react';
import { useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { DiffView } from '@/shared/ui/DiffView';
import { lineDiff } from '@/shared/lib/lineDiff';

import { skillVersionKey } from '../lib/skillVersionKey';
import type { SkillVersion } from '../model/types';

/**
 * The skill editor's "Compare versions" affordance (#874) — read-only,
 * side-by-side, mirroring `features/agents/ui/compare-versions/
 * CompareVersionsModal.tsx`'s own UX: the LEFT side is always the version
 * the editor currently has open, only the RIGHT side is chosen.
 *
 * **Simpler than the agent modal on purpose.** Agents' compare fetches each
 * side over the network (`useVersionDetail`, one GET per version) because an
 * `ApplicationVersionSummary` in the version LIST carries no content. A
 * skill's `versions[]` already carries every version's full
 * instructions/tags inline (`getSkillWithVersions` in
 * internal/infra/db/repos/skills.go) — the same array the version selector
 * itself renders from — so there is nothing to fetch and no loading state:
 * picking the right-hand version diffs immediately. That also means no
 * "Compare" button/two-phase flow — the diff is always live under the
 * selector.
 *
 * Reuses `shared/ui/DiffView` + `shared/lib/lineDiff` (the SAME line-diff
 * primitive `widgets/deepwiki`'s mermaid quick-fix uses) rather than
 * `features/agents`' word-level `TextDiffHighlight`: that component lives
 * inside `features/agents/ui/ai-edit/` and is not on that slice's public
 * `index.ts`, so importing it here would cross a feature boundary
 * `check-layer-cycle` refuses. `shared/ui/DiffView` is the sanctioned
 * cross-feature reuse point for exactly this kind of diff rendering.
 */
export interface SkillCompareModalProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly versions: readonly SkillVersion[];
  /** The version currently open in the editor — always the LEFT side. */
  readonly leftVersionKey: string;
}

function tagsDiffLine(tags: readonly string[]): string {
  return tags.length > 0 ? [...tags].sort().join(', ') : t('features.skills.compareVersions.noTags', '(none)');
}

/** One field's diff, or a "no differences" note — mirrors agents' ComparisonRow. */
function CompareField(props: { readonly label: string; readonly before: string; readonly after: string }): ReactNode {
  const { label, before, after } = props;
  const parts = useMemo(() => lineDiff(before, after), [before, after]);
  const noDiff = before === after;
  return (
    <Box sx={fieldSx}>
      <Typography variant="labelMedium">{props.label}</Typography>
      {noDiff ? (
        <Typography
          variant="bodySmall"
          color="text.secondary"
          sx={noDiffSx}
        >
          {t('features.skills.compareVersions.noDiff', 'No differences.')}
        </Typography>
      ) : (
        <DiffView
          parts={parts}
          data-testid={`skill-compare-diff-${label}`}
        />
      )}
    </Box>
  );
}

export function SkillCompareModal(props: SkillCompareModalProps): ReactNode {
  const { open, onClose, versions, leftVersionKey } = props;

  const left = versions.find((version) => skillVersionKey(version) === leftVersionKey);
  const availableVersions = useMemo(
    () => versions.filter((version) => skillVersionKey(version) !== leftVersionKey),
    [versions, leftVersionKey],
  );

  const [rightKey, setRightKey] = useState<string | undefined>(undefined);

  // Reopening (or the editor navigating to a different version) resets the
  // right-hand pick to the newest OTHER version, the same default agents'
  // CompareVersionsModal opens with.
  useEffect(() => {
    if (!open) return;
    setRightKey(availableVersions[0] !== undefined ? skillVersionKey(availableVersions[0]) : undefined);
    // availableVersions is derived from props that change every render;
    // re-running this on every render would stomp a selection the user just
    // made.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, leftVersionKey]);

  const right = versions.find((version) => skillVersionKey(version) === rightKey);

  const content = (
    <Box sx={contentSx}>
      <Box sx={selectionSx}>
        <Box>
          <Typography variant="labelMedium">
            {t('features.skills.compareVersions.baseVersion', 'Current version')}
          </Typography>
          <Typography color="text.secondary">{left?.name ?? ''}</Typography>
        </Box>
        <TextField
          select
          fullWidth
          label={t('features.skills.compareVersions.compareWith', 'Compare with')}
          value={rightKey ?? ''}
          onChange={(event) => setRightKey(event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'skill-compare-versions-select' } }}
        >
          {availableVersions.map((version) => (
            <MenuItem
              key={skillVersionKey(version)}
              value={skillVersionKey(version)}
            >
              {version.name}
            </MenuItem>
          ))}
        </TextField>
      </Box>
      {left && right && (
        <Box sx={fieldsSx}>
          <CompareField
            label={t('features.skills.compareVersions.instructions', 'Instructions')}
            before={left.instructions}
            after={right.instructions}
          />
          <CompareField
            label={t('features.skills.compareVersions.tags', 'Tags')}
            before={tagsDiffLine(left.tags)}
            after={tagsDiffLine(right.tags)}
          />
        </Box>
      )}
    </Box>
  );

  return (
    <BaseModal
      open={open}
      variant="complex"
      title={t('features.skills.compareVersions.title', 'Compare versions')}
      onClose={onClose}
      content={content}
      data-testid="skill-compare-versions-modal"
    />
  );
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.5rem', minWidth: '40rem' };
const selectionSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem' };
const fieldsSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.5rem' };
const fieldSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const noDiffSx: SxProps<Theme> = { fontStyle: 'italic' };
