/**
 * DWIKI-001 `/deepwiki` — the native wiki browser, and everything mounted
 * around it: generation, settings, the reader with its editor and mermaid
 * quick fix, delete, and the chat drawer.
 *
 * The page reads params and permission, renders error and pending, and
 * composes. It does not fetch: that is `features/wiki-browser`'s model, per
 * spec §3 (a page that fetches is a page that cannot be reused).
 *
 * BEHIND `BackendCapability.deepwiki`, WHICH IS ON. It was off while the
 * PROVIDER wrote wiki content through a path family elitea-main serves no
 * route in — the browser would have listed nothing, on a screen with no way
 * to tell that from "you have not generated a wiki yet". #665 fixed the
 * provider's client to write where this reads, so the flag flipped with it.
 *
 * WHAT THE FLAG DOES NOT BUY. Generating and chatting need the provider
 * SERVICE to be reachable through the facade; a deployment without one lists
 * and reads wikis and refuses to generate. That is a deployment fact, not a
 * capability this flag gates, and the generation panel reports it rather
 * than this page hiding.
 */
import { useMemo, useState } from 'react';
import Box from '@mui/material/Box';
import Stack from '@mui/material/Stack';
import type { SxProps, Theme } from '@mui/material/styles';

import { useWikiList, WikiList, WikiPageView } from '@/features/wiki-browser';
import { chatPinsFor, type RepositoryIdentity, type ToolkitSettings, type WikiManifest } from '@/entities/wiki';
import {
  DeleteWikiButton,
  WikiChatDrawer,
  WikiGenerationPanel,
  WikiPageEditor,
  WikiPageReader,
  WikiSettingsPanel,
  createWikiVersionStorage,
  type WikiChatTarget,
} from '@/widgets/deepwiki';
import { hasBackendCapability } from '@/shared/config/backendCapabilities';
import { t } from '@/shared/i18n';
import { AnimatedLoadingText } from '@/shared/ui/AnimatedLoadingText';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';

/** The provider's toolkit name — the SPI addresses it lowercased. */
const WIKI_TOOLKIT_NAME = 'wikis';

/**
 * The bucket a version choice is remembered under when this page is rendered
 * for no toolkit at all. Every route reaches here through `DeepWikiToolkit`,
 * which always has one; the sentinel exists so a toolkit-less render keeps its
 * own entry rather than sharing whichever toolkit happens to be `undefined`.
 */
const NO_TOOLKIT = 'none';

/**
 * The app's page recipe (pages/toolkits/Toolkits.tsx): the page owns its
 * height, its header bar and its scroll container, because the shell's
 * <main> supplies none of them and native scrollbars are hidden app-wide —
 * a page without `overflowY: 'auto'` here could not be scrolled at all.
 */
const pageSx: SxProps<Theme> = { height: '100%', display: 'flex', flexDirection: 'column' };
const tabBarSx: SxProps<Theme> = {
  flexShrink: 0,
  display: 'flex',
  alignItems: 'center',
  gap: 1,
  borderBottom: 1,
  borderColor: 'divider',
  padding: '0 1.5rem',
};
const panelsSx: SxProps<Theme> = { flexShrink: 0, gap: 2, padding: '1rem 1.5rem 0' };
const tabPanelSx: SxProps<Theme> = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '1.5rem' };

export interface DeepWikiProps {
  readonly projectId: string;
  readonly identity: RepositoryIdentity | null;
  readonly toolkitId?: string;
  readonly settings?: ToolkitSettings;
  /** The toolkit row as last read; the settings panel PUTs the whole of it back. */
  readonly toolkit?: Record<string, unknown>;
}

export function DeepWiki({
  projectId,
  identity,
  toolkitId,
  settings,
  toolkit,
}: DeepWikiProps): React.JSX.Element | null {
  // The CHOICE is stored, not the manifest: a manifest is a snapshot of one
  // listing, and holding it across a refetch would keep opening an object that
  // may no longer be in the bucket. The id is re-resolved against every
  // listing, which is also what makes a stale one fall back on its own.
  const versionStorage = useMemo(
    () => createWikiVersionStorage(projectId, toolkitId ?? NO_TOOLKIT),
    [projectId, toolkitId],
  );
  // Read on every render rather than seeded into `useState` once: the initial
  // value of a state hook is captured at MOUNT, so a route that swaps
  // `toolkitId` under the same component would go on using the previous
  // toolkit's choice. `null` here means "nothing chosen in this session", and
  // the stored value answers for it.
  const [chosenWikiId, setChosenWikiId] = useState<string | null>(null);
  const selectedWikiId = chosenWikiId ?? versionStorage.load();
  const [chatOpen, setChatOpen] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [editing, setEditing] = useState<{ pageKey: string; markdown: string } | null>(null);
  const enabled = hasBackendCapability('deepwiki');
  const query = useWikiList(projectId, identity, { enabled });

  if (!enabled) return null;
  if (query.isPending) {
    return (
      <Box sx={tabPanelSx}>
        <AnimatedLoadingText text={t('deepwiki.loading', 'Loading wikis…')} />
      </Box>
    );
  }
  if (query.isError) {
    return (
      <Box sx={tabPanelSx}>
        <BannerMessage
          variant="error"
          message={t('deepwiki.loadFailed', 'The wikis for this project could not be loaded.')}
        />
      </Box>
    );
  }

  // The wiki chosen last time is reopened when the listing STILL CONTAINS it,
  // otherwise the first one is. A stored id that no longer resolves — the
  // version was deleted, or the toolkit's repository moved — must land the
  // reader on a wiki rather than on nothing.
  //
  // The first wiki is what a project with no stored choice opens. A list that
  // needs a click before it shows anything reads as an empty screen on a
  // project with one wiki, which is the common case.
  const open = query.data.wikis.find((wiki) => wiki.wiki_id === selectedWikiId) ?? query.data.wikis[0];
  const chatTarget = chatTargetFor(projectId, toolkitId, settings, open);

  return (
    <Box sx={pageSx}>
      <ToolkitControls
        projectId={projectId}
        toolkitId={toolkitId}
        settings={settings}
        toolkit={toolkit}
        hasWiki={open !== undefined}
        settingsOpen={settingsOpen}
        onToggleSettings={() => {
          setSettingsOpen((v) => !v);
        }}
        chatAvailable={chatTarget !== null}
        onOpenChat={() => {
          setChatOpen(true);
        }}
      />
      <Box sx={tabPanelSx}>
        <WikiList
          wikis={query.data.wikis}
          allWikis={query.data.allWikis}
          // The OPEN wiki is the highlighted one, not only an explicitly
          // clicked one: a restored choice that reads back as unselected says
          // the list and the reader are showing different wikis.
          selectedWikiId={open?.wiki_id}
          onSelect={(wiki) => {
            setChosenWikiId(wiki.wiki_id ?? null);
            if (wiki.wiki_id !== undefined) versionStorage.save(wiki.wiki_id);
          }}
        />
        {open === undefined ? null : (
          <ReaderArea
            projectId={projectId}
            wiki={open}
            onDeleted={() => {
              // The stored choice is FORGOTTEN, not just dropped from state:
              // a deleted wiki left in storage would be looked up on every
              // later visit, and re-selected the moment a wiki with the same
              // id is generated again.
              setChosenWikiId(null);
              versionStorage.clear();
            }}
            onEdit={(pageKey, markdown) => {
              setEditing({ pageKey, markdown });
            }}
          />
        )}
      </Box>
      {editing === null ? null : (
        <WikiPageEditor
          open
          onClose={() => {
            setEditing(null);
          }}
          projectId={projectId}
          pageKey={editing.pageKey}
          markdown={editing.markdown}
        />
      )}
      {chatTarget === null ? null : (
        <WikiChatDrawer
          open={chatOpen}
          onClose={() => {
            setChatOpen(false);
          }}
          target={chatTarget}
          contextPages={open?.pages ?? []}
        />
      )}
    </Box>
  );
}

interface ToolkitControlsProps {
  readonly projectId: string;
  readonly toolkitId: string | undefined;
  readonly settings: ToolkitSettings | undefined;
  readonly toolkit: Record<string, unknown> | undefined;
  readonly hasWiki: boolean;
  readonly settingsOpen: boolean;
  readonly onToggleSettings: () => void;
  readonly chatAvailable: boolean;
  readonly onOpenChat: () => void;
}

/** The header bar — the page's name and its actions — and the panels under it. */
function ToolkitControls({
  projectId,
  toolkitId,
  settings,
  toolkit,
  hasWiki,
  settingsOpen,
  onToggleSettings,
  chatAvailable,
  onOpenChat,
}: ToolkitControlsProps): React.JSX.Element {
  const hasToolkit = toolkitId !== undefined && settings !== undefined;
  return (
    <>
      <Box sx={tabBarSx}>
        <BaseTabs value={0} sx={{ flex: 1 }}>
          <BaseTab label={t('deepwiki.title', 'Wiki')} />
        </BaseTabs>
        {hasToolkit ? (
          <BaseBtn variant="tertiary" size="small" onClick={onToggleSettings} data-testid="wiki-settings-toggle">
            {settingsOpen ? t('deepwiki.settings.hide', 'Hide settings') : t('deepwiki.settings.show', 'Settings')}
          </BaseBtn>
        ) : null}
        {chatAvailable ? (
          <BaseBtn variant="secondary" size="small" onClick={onOpenChat}>
            {t('deepwiki.openChat', 'Ask about this repository')}
          </BaseBtn>
        ) : null}
      </Box>
      {hasToolkit ? (
        <Stack sx={panelsSx}>
          {settingsOpen && toolkit !== undefined ? (
            <WikiSettingsPanel projectId={projectId} toolkitId={toolkitId} toolkit={toolkit} settings={settings} />
          ) : null}
          <WikiGenerationPanel projectId={projectId} toolkitId={toolkitId} settings={settings} hasWiki={hasWiki} />
        </Stack>
      ) : null}
    </>
  );
}

interface ReaderAreaProps {
  readonly projectId: string;
  readonly wiki: WikiManifest;
  readonly onDeleted: () => void;
  readonly onEdit: (pageKey: string, markdown: string) => void;
}

function ReaderArea({ projectId, wiki, onDeleted, onEdit }: ReaderAreaProps): React.JSX.Element {
  return (
    <Box sx={{ mt: 1 }}>
      <Stack sx={{ flexDirection: 'row', gap: 1, mb: 1 }}>
        <DeleteWikiButton projectId={projectId} wiki={wiki} onDeleted={onDeleted} />
      </Stack>
      <WikiPageView
        projectId={projectId}
        wiki={wiki}
        renderContent={(markdown, pageKey) => (
          <Box>
            <Stack sx={{ flexDirection: 'row', justifyContent: 'flex-end', mb: 0.5 }}>
              <BaseBtn
                variant="tertiary"
                size="small"
                onClick={() => {
                  onEdit(pageKey, markdown);
                }}
                data-testid="wiki-page-edit"
              >
                {t('deepwiki.editor.open', 'Edit page')}
              </BaseBtn>
            </Stack>
            <WikiPageReader projectId={projectId} pageKey={pageKey} markdown={markdown} />
          </Box>
        )}
      />
    </Box>
  );
}

/**
 * The chat is pinned to the OPEN wiki's own identifiers (`chatPinsFor`), never
 * to the toolkit's repository name: the override is a cache key, and a bare
 * repository name is a key nothing was indexed under.
 */
function chatTargetFor(
  projectId: string,
  toolkitId: string | undefined,
  settings: ToolkitSettings | undefined,
  open: WikiManifest | undefined,
): WikiChatTarget | null {
  if (toolkitId === undefined || settings === undefined) return null;
  return {
    projectId: Number(projectId),
    toolkitId: Number(toolkitId),
    toolkitName: WIKI_TOOLKIT_NAME,
    toolkitType: WIKI_TOOLKIT_NAME,
    settings,
    ...chatPinsFor(open),
    // The VERSION pin for an attachment. It is deliberately the open
    // manifest's own id rather than "whatever is newest": a page attached to
    // a question must resolve against the version the reader was reading,
    // even if a regeneration lands while they are typing.
    ...(open?.wiki_version_id ? { wikiVersionId: open.wiki_version_id } : {}),
  };
}
