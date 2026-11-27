/**
 * `/marketplace/{kind}/{listingId}`: one listing as a page (ADR-0051 decision
 * 8). The kind picks the page; an unknown kind reads as agents, the same
 * degrade `/marketplace?kind=` applies.
 */
import type { ReactNode } from 'react';

import { useParams } from '@tanstack/react-router';

import { kindFromSearch } from './listing';
import { AgentListing } from './ui/AgentListing';
import { SkillListing } from './ui/SkillListing';
import { TeamListing } from './ui/TeamListing';

interface ListingParams {
  kind?: string;
  listingId?: string;
}

export function MarketplaceListing(): ReactNode {
  const params = useParams({ strict: false }) as ListingParams;
  const listingId = params.listingId ?? '';
  const kind = kindFromSearch(params.kind);
  if (kind === 'skills') return <SkillListing listingId={listingId} />;
  if (kind === 'teams') return <TeamListing listingId={listingId} />;
  return <AgentListing listingId={listingId} />;
}
