/**
 * "Start from a blueprint" (issue 3137): the gallery of ready agents and
 * teams the build ships (`<base>/blueprints/`), opened from the Agents and
 * Teams side panels next to Import. A blueprint is imported through the SAME
 * reader, dialog and write as Import, so it lands exactly as its file would;
 * or crafted from, which opens Build with AI with the blueprint's brief.
 *
 * Its own widget, not part of the Agents or Teams page: it composes the
 * import (agent-lifecycle), Build with AI (agents) and the avatar renderer
 * (chat-messages), which a feature may not import sideways.
 */
import type { ReactNode, SyntheticEvent } from 'react';
import { useState } from 'react';

import AutoAwesomeMosaicOutlinedIcon from '@mui/icons-material/AutoAwesomeMosaicOutlined';

import { ImportDialog, useEntityImport } from '@/features/agent-lifecycle';
import { GenerateAgentModal } from '@/features/agents';
import { hasBackendCapability } from '@/shared/config';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { BlueprintEntry } from '../model/blueprintIndex';
import { useBlueprintImport } from '../model/useBlueprintImport';
import { BlueprintGalleryBody, type GalleryTab } from './BlueprintGalleryBody';

export interface BlueprintGalleryButtonProps {
  readonly projectId: string | undefined;
  /** The tab the gallery opens on: the Agents page opens Agents, the Teams page Teams. */
  readonly initialTab: GalleryTab;
  /** Distinguishes the copies in the DOM (the Teams page shows one in its side panel and one in its header). */
  readonly testIdPrefix: string;
  /** Called with the id of the agent Craft from it created. */
  readonly onAgentCreated: (agentId: string) => void;
}

export function BlueprintGalleryButton({ projectId, initialTab, testIdPrefix, onAgentCreated }: BlueprintGalleryButtonProps): ReactNode {
  const [open, setOpen] = useState(false);
  const [tab, setTab] = useState<GalleryTab>(initialTab);
  const [crafting, setCrafting] = useState<BlueprintEntry>();
  const blueprintImport = useBlueprintImport();
  const entityImport = useEntityImport(projectId);
  const canCraft = hasBackendCapability('aiGeneration');

  return (
    <>
      <BaseBtn
        variant="secondary"
        startIcon={<AutoAwesomeMosaicOutlinedIcon />}
        data-testid={`${testIdPrefix}-blueprints-open`}
        onClick={() => {
          setTab(initialTab);
          setOpen(true);
        }}
      >
        {t('widgets.blueprintGallery.open', 'Start from a blueprint')}
      </BaseBtn>
      <BaseModal
        open={open}
        variant="complex"
        fullscreen="wide"
        title={t('widgets.blueprintGallery.title', 'Start from a blueprint')}
        data-testid="blueprint-gallery"
        onClose={() => setOpen(false)}
        actions={{ node: <BaseBtn variant="secondary" onClick={() => setOpen(false)}>{t('widgets.blueprintGallery.close', 'Close')}</BaseBtn> }}
        content={
          <BlueprintGalleryBody
            enabled={open}
            tab={tab}
            onTabChange={(_event: SyntheticEvent, value: GalleryTab) => setTab(value)}
            loadingSlug={blueprintImport.loadingSlug}
            importError={blueprintImport.error}
            onImport={(entry) => void blueprintImport.start(entry)}
            onCraft={canCraft ? setCrafting : undefined}
          />
        }
      />
      <ImportDialog
        pending={blueprintImport.pending}
        isImporting={entityImport.run.isPending}
        onImport={(request) => entityImport.run.mutateAsync(request)}
        onClose={blueprintImport.close}
        testIdPrefix="blueprints"
      />
      {crafting !== undefined && (
        <GenerateAgentModal
          key={crafting.slug}
          open
          initialPrompt={crafting.brief}
          projectId={projectId}
          onClose={() => setCrafting(undefined)}
          onAgentCreated={(result) => {
            setOpen(false);
            onAgentCreated(String(result.id));
          }}
        />
      )}
    </>
  );
}
