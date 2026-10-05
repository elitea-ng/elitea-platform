import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { ApplicationsIcon } from '@/shared/ui/icons/applications-icon';
import { FlowIcon } from '@/shared/ui/icons/flow-icon';
import { McpIcon } from '@/shared/ui/icons/mcp-icon';
import { SkillIcon } from '@/shared/ui/icons/skill-icon';
import { ToolIcon } from '@/shared/ui/icons/tool-icon';

import type { SuggestedResource } from '../../lib/agentDraft';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent/ui/generate-agent-modal/SuggestionItem.jsx`.
 *
 * **DISCLOSED SIMPLIFICATION, matching an established precedent —
 * `EntityIcon` dropped.** The baseline wraps its icon in `<EntityIcon icon
 * entityType specifiedFontSize="1rem" />` (`components/EntityIcon.jsx`), a
 * large, editable, gradient-ring icon component this app has no port of.
 * `src/features/apps/ui/catalog/ApplicationCatalogCard.tsx`'s own doc
 * comment already establishes the precedent for this exact situation
 * ("Dropped from the baseline: the `EntityIcon`/`HighlightQuery` wrapper
 * components... this port renders `application.Icon` directly, no ring/
 * editable affordance needed for a static, non-editable... entry") — this
 * suggestion-list row is equally static and non-editable (no
 * `editable`/`onChangeIcon` prop was ever passed at the baseline's own
 * call site either), so the same call applies. `EntityTypeIcon`'s
 * (`EntityIcon.jsx:39-131`) per-`entityType` default-icon switch is
 * reproduced with the icons this app HAS ported (`applications`/`flow`/
 * `mcp`/`skill`); a per-toolkit-brand icon lookup
 * (`common/toolkitUtils.jsx`'s `getToolIconByType`, ~30 brand SVGs) is out
 * of scope for this leaf row — `toolkit` items fall back to the generic
 * `ToolIcon`, same "drop the decorative fanciness, keep the function"
 * call.
 */

export interface SuggestionItemProps {
  readonly item: SuggestedResource;
  readonly checked: boolean;
  readonly onToggle: (id: number | string) => void;
  readonly entityType: 'toolkit' | 'mcp' | 'pipeline' | 'agent' | 'skill';
}

function defaultIconFor(entityType: SuggestionItemProps['entityType']): ReactNode {
  switch (entityType) {
    case 'agent':
      return <ApplicationsIcon />;
    case 'pipeline':
      return (
        <Box
          component={FlowIcon}
          sx={iconBoxSx}
        />
      );
    case 'mcp':
      return <McpIcon />;
    case 'skill':
      return (
        <Box
          component={SkillIcon}
          sx={iconBoxSx}
        />
      );
    case 'toolkit':
    default:
      return <ToolIcon />;
  }
}

export function SuggestionItem({ item, checked, onToggle, entityType }: SuggestionItemProps): ReactNode {
  const secondaryText = entityType === 'toolkit' ? item.type : item.description;
  const showSecondary = Boolean(secondaryText) && secondaryText !== item.name;
  const icon = useMemo(() => defaultIconFor(entityType), [entityType]);

  const handleToggle = () => onToggle(item.id);

  return (
    <Box
      sx={itemSx}
      onClick={handleToggle}
    >
      <BaseCheckbox
        size="small"
        checked={checked}
        onChange={handleToggle}
        onClick={(event) => event.stopPropagation()}
        aria-label={item.name}
        sx={checkboxSx}
      />
      <Box sx={cardSx}>
        {icon}
        <Box sx={cardContentSx}>
          <Typography
            variant="bodyMedium"
            sx={itemNameSx}
            noWrap
          >
            {item.name}
          </Typography>
          {showSecondary && (
            <Typography
              variant="bodySmall"
              sx={secondaryTextSx}
              noWrap
            >
              {secondaryText}
            </Typography>
          )}
        </Box>
      </Box>
    </Box>
  );
}

const iconBoxSx: SxProps<Theme> = { width: '1rem', height: '1rem' };

const itemSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '1rem', cursor: 'pointer' };

const checkboxSx: SxProps<Theme> = { padding: '0.25rem', flexShrink: 0 };

const cardSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: '1rem',
  flex: 1,
  minWidth: 0,
  padding: '0.5rem 1rem',
  borderRadius: theme.vars.shape.radiusLg,
  backgroundColor: theme.vars.palette.background.userInputBackground,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
});

const cardContentSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  justifyContent: 'center',
  minWidth: 0,
  flex: 1,
};

const itemNameSx: SxProps<Theme> = { color: 'text.secondary' };

const secondaryTextSx: SxProps<Theme> = { color: 'text.primary' };
