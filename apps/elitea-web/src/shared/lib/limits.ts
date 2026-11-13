/**
 * Numeric pagination/validation-length limits ported from
 * apps/elitea-ui/src/common/constants.js (unit S3, spec §9.3).
 */
import { DESCRIPTION_MAX_LENGTH } from '@elitea/client-core/limits';

export const POSITION_GAP = 1_000_000;
export const MIN_SEARCH_KEYWORD_LENGTH = 3;
export const PAGE_SIZE = 20;
export const PAGE_SIZE_TOOLKITS_DROPDOWN_LIST = 50;
export const SUGGESTION_PAGE_SIZE = 5;
export const TOAST_DURATION = 3000;

export const TAG_NAME_MAX_LENGTH = 48;
export const MAX_NAME_LENGTH = 32;
/** Issue 1968: the contract's description limit (2000), shared with the server and mobile. */
export const MAX_DESCRIPTION_LENGTH = DESCRIPTION_MAX_LENGTH;
export const MAX_INSTRUCTIONS_LENGTH = 2500;
export const MAX_VARIABLES_LENGTH = 768;
export const MAX_STEP_LIMIT = 999;
export const MIN_STEP_LIMIT = 0;
export const MAX_VERSION_LENGTH = 20;
export const MAX_CONVERSATION_LENGTH = 50;
export const MAX_CONVERSATION_STARTERS = 4;
export const MAX_CONVERSATION_STARTER_LENGTH = 768;
export const MAX_WELCOME_MESSAGE_LENGTH = 768;
