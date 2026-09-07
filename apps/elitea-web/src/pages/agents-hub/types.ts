/**
 * Agent Hub types — shared interfaces for the agents-hub cluster.
 *
 * @public Wave-2 unit A13 surface.
 */
import type { PublicApplicationSummary } from '@/shared/api/generated/model';

import { LikeUpdateStrategy } from './constants';

/* ── Application shape (extends generated type with hub-specific fields) ─ */

/**
 * `tags`, `likes` and `is_liked` are NOT redeclared here: the catalogue row
 * carries all three now (`PublicApplicationSummary` in the generated model),
 * so an optional copy on this interface would let a card read `undefined`
 * from a field the server always sends.
 */
export interface ApplicationData extends PublicApplicationSummary {
  category?: string;
  authors?: AuthorData[];
  author?: AuthorData;
  icon_meta?: Record<string, unknown> | null;
  version_details?: VersionDetails;
  welcome_message?: string;
  conversation_starters?: string[];
}

export interface AuthorData {
  id: string;
  name: string;
  username?: string;
}

interface VersionDetails {
  id: string;
  author?: AuthorData;
  icon_meta?: Record<string, unknown> | null;
  welcome_message?: string;
  conversation_starters?: string[];
  instructions?: string;
  llm_settings?: Record<string, unknown>;
  agent_type?: string;
  variables?: Record<string, unknown>;
}

/* ── Strategy enum ────────────────────────────────────────────────────── */

export type LikeUpdateStrategyValue = (typeof LikeUpdateStrategy)[keyof typeof LikeUpdateStrategy];

/* ── Catalogue query options ─────────────────────────────────────────── */

/**
 * The sort keys the catalogue accepts. Anything else is a 400 from the
 * handler, so this union is the server's allowlist, not a wish
 * (`services/elitea-main/internal/api/v2/eliteacore/public_applications.go`).
 */
export type AgentHubSortBy = 'created_at' | 'name' | 'likes' | 'id';
export type AgentHubSortOrder = 'asc' | 'desc';

export interface AgentHubQueryOptions {
  /** Free-text search, sent as `?query=` and matched over name and description. */
  readonly query?: string;
  readonly sortBy?: AgentHubSortBy;
  readonly sortOrder?: AgentHubSortOrder;
}
