/**
 * An agent's listing page (ADR-0051 decision 8): who it is, what it does, what
 * it needs, its versions with release notes, and the actions **Hire** (a split
 * button: Install or Fork) and **Try in chat** (a conversation with the public
 * copy).
 *
 * The page reads ONE listing by id through the listing route, which carries the
 * card, the versions and the hires of this project. It does not read the paged
 * catalogue list.
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
import type { MarketplaceListingDetail, PublicApplicationSummary } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { handleCopy } from '@/shared/lib/clipboard';
import { CopyLinkIcon } from '@/shared/ui/icons/copy-link-icon';
import { usePageCrumbs } from '@/shared/ui/page-chrome';
import { useSelectedProjectId } from '@/pages/skills/lib/useSelectedProjectId';

import { publicApplicationOf } from '../listing';
import { useScrollToHash } from '../useScrollToHash';
import { useAgentVersionDetail } from '../useAgentVersionDetail';
import { useMarketplaceListing } from '../useMarketplaceListings';
import { useTryInChat, type TryInChat } from '../useTryInChat';

import { AgentConversationStarters } from './AgentConversationStarters';
import { AgentDescription } from './AgentDescription';
import AgentHubLike from './AgentHubLike';
import { AgentWelcomeMessage } from './AgentWelcomeMessage';
import { HireButton } from './HireButton';
import { ListingLoadError } from './ListingLoadError';
import { ListingNotFound } from './ListingNotFound';
import { ListingRequirements } from './ListingRequirements';
import { ListingFeedback } from '../feedback/ListingFeedback';
import { DelistedBanner, ListingAdminBar, ListingUsedBy } from './ListingSections';
import { ListingBlock, ListingShell } from './ListingShell';
import { ListingVersions } from './ListingVersions';

interface AgentListingViewProps {
  readonly detail: MarketplaceListingDetail;
  readonly projectId: string | undefined;
}

/** Beside the name: the like and the link to this page. */
function AgentMeta({
  app,
  listingId,
}: {
  readonly app: PublicApplicationSummary;
  readonly listingId: string;
}): ReactNode {
  const router = useRouter();
  const copyLink = useCallback(() => {
    const href = router.buildLocation({
      to: '/marketplace/$kind/$listingId',
      params: { kind: 'agents', listingId },
    }).href;
    void handleCopy(`${window.location.origin}${href}`);
  }, [router, listingId]);
  return (
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
  );
}

/** The page's two actions: Hire, and Try in chat with the public copy. */
function AgentActions({
  detail,
  projectId,
  app,
  tryInChat,
}: {
  readonly detail: MarketplaceListingDetail;
  readonly projectId: string | undefined;
  readonly app: PublicApplicationSummary;
  readonly tryInChat: TryInChat;
}): ReactNode {
  return (
    <>
      <HireButton projectId={projectId} detail={detail} />
      <Button
        variant="outlined"
        onClick={tryInChat.start}
        disabled={tryInChat.start === undefined || tryInChat.isStarting || app.version_id === ''}
        data-testid="marketplace-try-in-chat"
      >
        {t('marketplace.listing.tryInChat', 'Try in chat')}
      </Button>
    </>
  );
}

function AgentOverview({
  detail,
  onShowContext,
}: {
  readonly detail: MarketplaceListingDetail;
  readonly onShowContext: () => void;
}): ReactNode {
  const { listing } = detail;
  const publisher =
    listing.publisher === null || listing.publisher === undefined
      ? ''
      : ` · ${t('marketplace.card.publisher', 'by {{name}}', { name: listing.publisher.name })}`;
  return (
    <ListingBlock id="overview" title={t('marketplace.listing.overview', 'Overview')}>
      <Typography variant="bodySmall" color="text.secondary">
        {t('marketplace.listing.category', 'Category: {{category}}', {
          category: listing.category || t('marketplace.listing.noCategory', 'None'),
        })}
        {publisher}
      </Typography>
      <AgentDescription
        description={listing.description || t('marketplace.listing.noDescription', 'No description available.')}
        isSmallHeight
      />
      {detail.soul_summary !== undefined && detail.soul_summary !== '' && (
        <Typography variant="bodySmall">{detail.soul_summary}</Typography>
      )}
      <Button
        size="small"
        onClick={onShowContext}
        sx={{ alignSelf: 'flex-start' }}
        data-testid="marketplace-show-context"
      >
        {t('marketplace.listing.showContext', 'Show context')}
      </Button>
    </ListingBlock>
  );
}

/** Requirements, starters, welcome message, versions, feedback and what this project hired. */
function AgentSections({
  detail,
  projectId,
  onStart,
}: {
  readonly detail: MarketplaceListingDetail;
  readonly projectId: string | undefined;
  readonly onStart: () => void;
}): ReactNode {
  const { listing } = detail;
  const current = detail.versions.find((version) => version.id === listing.current_version?.id);
  return (
    <>
      {current !== undefined && (
        <ListingBlock id="requirements" title={t('marketplace.listing.requirements', 'Requirements')}>
          <ListingRequirements requirements={current.requirements} />
        </ListingBlock>
      )}
      <ListingBlock id="starters" title={t('marketplace.listing.starters', 'Conversation starters')}>
        <AgentConversationStarters
          conversation_starters={detail.conversation_starters ?? []}
          onSelectStarter={onStart}
        />
      </ListingBlock>
      <ListingBlock id="welcome" title={t('marketplace.listing.welcome', 'Welcome message')}>
        <AgentWelcomeMessage welcome_message={detail.welcome_message ?? ''} />
      </ListingBlock>
      <ListingBlock id="versions" title={t('marketplace.listing.versions', 'Versions')}>
        <ListingVersions
          projectId={projectId}
          listingId={listing.id}
          versions={detail.versions}
          canManage={detail.can_manage}
          canModerate={detail.can_moderate}
        />
      </ListingBlock>
      <ListingBlock id="feedback" title={t('marketplace.listing.feedback', 'Feedback')}>
        <ListingFeedback detail={detail} projectId={projectId} />
      </ListingBlock>
      {listing.hired.length > 0 && (
        <ListingBlock id="used-by" title={t('marketplace.listing.usedBy', 'Used by this project')}>
          <ListingUsedBy hired={listing.hired} kind="agents" />
        </ListingBlock>
      )}
    </>
  );
}

function AgentListingView({ detail, projectId }: AgentListingViewProps): ReactNode {
  const queryClient = useQueryClient();
  const { listing } = detail;
  const app = useMemo(() => publicApplicationOf(listing), [listing]);
  const tryInChat = useTryInChat(app);
  const { versionDetails, isLoading } = useAgentVersionDetail(listing.entity_id, app.version_name);
  const [showContext, setShowContext] = useState(false);
  useScrollToHash();

  // A like given here changes the count the grid shows; the grid refetches when next opened.
  useEffect(() => () => void queryClient.invalidateQueries({ queryKey: ['marketplace'] }), [queryClient]);

  return (
    <>
      <ListingShell
        name={listing.name}
        iconUrl={listing.icon_url ?? undefined}
        version={listing.current_version?.name}
        meta={<AgentMeta app={app} listingId={listing.id} />}
        actions={<AgentActions detail={detail} projectId={projectId} app={app} tryInChat={tryInChat} />}
      >
        <DelistedBanner detail={detail} />
        <ListingAdminBar projectId={projectId} detail={detail} />
        {tryInChat.isError && (
          <Alert severity="error">
            {t('marketplace.listing.tryFailed', 'The conversation could not be started. Try again.')}
          </Alert>
        )}
        <AgentOverview detail={detail} onShowContext={() => setShowContext(true)} />
        <AgentSections detail={detail} projectId={projectId} onStart={() => tryInChat.start?.()} />
      </ListingShell>
      {showContext && (
        <StyledShowContextModal
          context={versionDetails?.instructions ?? ''}
          open
          onClose={() => setShowContext(false)}
          isLoading={isLoading}
        />
      )}
    </>
  );
}

export function AgentListing({ listingId }: { readonly listingId: string }): ReactNode {
  const projectId = useSelectedProjectId();
  const { detail, isPending, isNotFound, isError, refetch } = useMarketplaceListing(projectId, listingId);
  usePageCrumbs(detail === undefined ? [] : [{ label: detail.listing.name }]);

  if (isPending) return <LinearProgress />;
  if (isError) return <ListingLoadError onRetry={refetch} />;
  if (detail === undefined || isNotFound) return <ListingNotFound />;
  return <AgentListingView detail={detail} projectId={projectId} />;
}
