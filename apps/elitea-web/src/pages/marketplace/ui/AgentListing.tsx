/**
 * An agent's listing page: who it is, what it does, how it starts, and
 * **Try in chat** (today's Start conversation: a conversation with the public
 * copy).
 *
 * Hiring — Install and Fork, versions with release notes, feedback — arrives in
 * later phases of ADR-0051 and is not drawn here.
 */
import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import LinearProgress from '@mui/material/LinearProgress';
import Typography from '@mui/material/Typography';

import { useQueryClient } from '@tanstack/react-query';
import { useRouter } from '@tanstack/react-router';

import { StyledShowContextModal } from '@/features/agents';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { handleCopy } from '@/shared/lib/clipboard';
import { CopyLinkIcon } from '@/shared/ui/icons/copy-link-icon';
import { usePageCrumbs } from '@/shared/ui/page-chrome';

import { getCategoryForApplication } from '../helpers';
import { agentCard } from '../listing';
import type { ApplicationData } from '../types';
import { useAgentVersionDetail } from '../useAgentVersionDetail';
import { useTryInChat } from '../useTryInChat';
import { useAgentRows } from '../useMarketplaceListings';

import { AgentConversationStarters } from './AgentConversationStarters';
import { AgentDescription } from './AgentDescription';
import AgentHubLike from './AgentHubLike';
import { AgentWelcomeMessage } from './AgentWelcomeMessage';
import { ListingBlock, ListingShell } from './ListingShell';
import { ListingNotFound } from './ListingNotFound';

/** The wire types the starters as opaque (`unknown[]`); keep the strings. */
function toStringArray(value: readonly unknown[] | undefined): string[] {
  return (value ?? []).filter((item): item is string => typeof item === 'string');
}

interface AgentListingViewProps {
  readonly app: ApplicationData;
  readonly versionDetails: ApplicationVersionDetail | undefined;
  readonly isVersionLoading: boolean;
}

function AgentListingView({ app, versionDetails, isVersionLoading }: AgentListingViewProps): ReactNode {
  const router = useRouter();
  const queryClient = useQueryClient();
  const tryInChat = useTryInChat(app);
  const [showContext, setShowContext] = useState(false);
  const card = agentCard(app);

  const starters = useMemo(() => toStringArray(versionDetails?.conversation_starters), [versionDetails]);
  // A like given here changes the count the grid and the Popular row show; they refetch when next opened.
  useEffect(() => () => void queryClient.invalidateQueries({ queryKey: ['marketplace', 'agents'] }), [queryClient]);
  const copyLink = useCallback(() => {
    const href = router.buildLocation({ to: '/marketplace/$kind/$listingId', params: { kind: 'agents', listingId: card.id } }).href;
    void handleCopy(`${window.location.origin}${href}`);
  }, [router, card.id]);

  return (
    <>
      <ListingShell
        name={card.name}
        iconUrl={card.iconUrl}
        version={card.version}
        meta={
          <>
            <AgentHubLike data={app} />
            <IconButton
              size="small"
              aria-label={t('marketplace.listing.copyLink', 'Copy link to this listing')}
              data-testid="marketplace-copy-link"
              onClick={copyLink}
            >
              <CopyLinkIcon width="16" height="16" />
            </IconButton>
          </>
        }
        actions={
          <Button variant="contained" onClick={tryInChat.start} disabled={tryInChat.start === undefined || tryInChat.isStarting} data-testid="marketplace-try-in-chat">
            {t('marketplace.listing.tryInChat', 'Try in chat')}
          </Button>
        }
      >
        {tryInChat.isError && <Alert severity="error">{t('marketplace.listing.tryFailed', 'The conversation could not be started. Try again.')}</Alert>}
        <ListingBlock id="overview" title={t('marketplace.listing.overview', 'Overview')}>
          <Typography variant="bodySmall" color="text.secondary">
            {t('marketplace.listing.category', 'Category: {{category}}', { category: getCategoryForApplication(app) })}
          </Typography>
          <AgentDescription description={card.description || t('marketplace.listing.noDescription', 'No description available.')} isSmallHeight />
          <Button size="small" onClick={() => setShowContext(true)} sx={{ alignSelf: 'flex-start' }} data-testid="marketplace-show-context">
            {t('marketplace.listing.showContext', 'Show context')}
          </Button>
        </ListingBlock>
        <ListingBlock id="starters" title={t('marketplace.listing.starters', 'Conversation starters')}>
          <AgentConversationStarters conversation_starters={starters} onSelectStarter={() => tryInChat.start?.()} />
        </ListingBlock>
        <ListingBlock id="welcome" title={t('marketplace.listing.welcome', 'Welcome message')}>
          <AgentWelcomeMessage welcome_message={versionDetails?.welcome_message ?? ''} />
        </ListingBlock>
      </ListingShell>
      {showContext && (
        <StyledShowContextModal context={versionDetails?.instructions ?? ''} open onClose={() => setShowContext(false)} isLoading={isVersionLoading} />
      )}
    </>
  );
}

export function AgentListing({ listingId }: { readonly listingId: string }): ReactNode {
  const rows = useAgentRows('', 'newest');
  const app = rows.data?.rows.find((row) => String(row.id) === listingId);
  const { versionDetails, isLoading } = useAgentVersionDetail(app === undefined ? '' : String(app.id), app?.version_name ?? '');
  usePageCrumbs(app === undefined ? [] : [{ label: app.name }]);

  if (rows.isPending) return <LinearProgress />;
  if (rows.isError) return <Alert severity="error">{t('marketplace.loadError', 'The listings did not load. Reload the page to try again.')}</Alert>;
  if (app === undefined) return <ListingNotFound />;
  return <AgentListingView app={app} versionDetails={versionDetails} isVersionLoading={isLoading} />;
}
