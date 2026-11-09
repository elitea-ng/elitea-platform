/**
 * User-visible action/entity-name copy ported from
 * apps/elitea-ui/src/common/constants.js (unit S3, spec §9.3).
 *
 * Every value in this file is user-visible copy. Per R-T3/N3 these should
 * ultimately flow through unit S8's `t()`/`shared/i18n/en.json` bundle
 * rather than live as plain TS constants — ported here verbatim (values
 * unchanged) only as a parity floor until S8 lands and Wave-2 feature units
 * have something to reference now. Not a recommendation to keep hand-written
 * string constants permanently; flagged once here rather than repeated
 * per-export.
 */
import { productName } from '@/shared/brand';

export const SAVE = 'Save';
export const CREATE_VERSION = 'Create version';

export const PUBLIC_PROJECT_NAME = 'Public';
export const PRIVATE_PROJECT_NAME = 'Private';

/** The assistant's name when no sender name is configured: the product's (ADR-0052 W3.3). */
export const DEFAULT_PARTICIPANT_NAME = productName();

export const DefaultConversationName = 'New Chat';
export const DefaultFolderName = 'New folder';
