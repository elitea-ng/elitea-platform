/**
 * pages/credentials/CredentialsTypesPanel.tsx — the credentials list's
 * Filters tab in the shell's side panel: the credential types of the loaded
 * rows, as toggle chips. Ported from
 * `apps/elitea-ui/src/pages/Credentials/CredentialsTypesPanel.jsx`.
 *
 * The same chip panel every list page's Filters shows (`shared/ui/
 * EntityRail`'s `RailTagsPanelView`): chips are labelled with the type's
 * display name and its credential count, and toggle the underlying type. The selection is the caller's
 * state (`CredentialsList.tsx`): the credentials routes validate no `tags`
 * search param (PARAM-037..046).
 *
 * `CredentialTag`'s shape is inlined rather than imported from
 * `@/features/credentials` — that slice's public-API budget (§3.5, ≤20) is
 * already at its limit; this is a read-only structural subset of
 * `generateCredentialTagList`'s real return type.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';

import { t } from '@/shared/i18n';
import { filtersPanelSx, RailTagsPanelView } from '@/shared/ui/EntityRail';
import { SidePanelTab } from '@/shared/ui/side-panel';

export interface CredentialTypeTag {
  readonly id: string;
  readonly name: string;
  readonly data: { readonly type: string };
  readonly credentialCount: number;
}

export interface CredentialsTypesPanelProps {
  readonly tagList: readonly CredentialTypeTag[];
  readonly selectedTypes: readonly string[];
  readonly onToggleType: (type: string) => void;
  readonly onClearTypes: () => void;
  readonly isLoading: boolean;
}

export function CredentialsTypesPanel({ tagList, selectedTypes, onToggleType, onClearTypes, isLoading }: CredentialsTypesPanelProps): ReactNode {
  const chips = tagList.map((tag) => ({ id: tag.id, name: `${tag.name} (${String(tag.credentialCount)})`, type: tag.data.type }));
  const typeByLabel = new Map(chips.map((chip) => [chip.name, chip.type]));
  const selectedLabels = chips.filter((chip) => selectedTypes.includes(chip.type)).map((chip) => chip.name);

  return (
    <SidePanelTab
      id="filters"
      title={t('shell.sidePanel.filters', 'Filters')}
      subject={t('credentials.page.title', 'Credentials')}
    >
      <Box
        data-testid="credentials-filters"
        sx={filtersPanelSx}
      >
        <RailTagsPanelView
          title={t('pages.credentials.types', 'Types')}
          emptyText={t('pages.credentials.noTypes', 'No types to display.')}
          tags={chips}
          selectedTags={selectedLabels}
          onToggleTag={(label) => {
            const type = typeByLabel.get(label);
            if (type !== undefined) onToggleType(type);
          }}
          onClearTags={onClearTypes}
          isLoading={isLoading}
          isError={false}
        />
      </Box>
    </SidePanelTab>
  );
}
