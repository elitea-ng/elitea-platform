/**
 * Layout and safety constants for the grouped RunStateDialog timeline
 * (`EL-3734` / `#1585`, ported from `EliteaUI` `timeLine.constants.js` +
 * replatform hierarchy/error bounds).
 */

export const STEP_MIN_WIDTH = '6.25rem';
export const STEP_CONNECTOR_MIN_WIDTH = '2.6875rem';
/** A step name may outgrow its step, but never past the gap to the neighbouring dots. */
export const STEP_NAME_MAX_WIDTH = `calc(${STEP_MIN_WIDTH} + ${STEP_CONNECTOR_MIN_WIDTH} - 0.5rem)`;
/** The first and last steps sit near the scroll edges and use a shorter step width. */
export const STEP_NAME_EDGE_MAX_WIDTH = '6rem';
export const GROUP_TOGGLE_BUTTON_HEIGHT = '1.75rem';
/** Larger counts collapse to `99+`, so a big group cannot squeeze its name out of the step. */
export const MAX_BADGE_COUNT = 99;
/** Matches `entities/message` hierarchy normalization depth cap. */
export const MAX_HIERARCHY_DEPTH = 3;
/** Matches `nodeSettings.helpers.ts` (`max_node_errors: 64`) bound per step scan. */
export const MAX_NODE_ERRORS_SCAN = 64;
/** Upper bound on normalized hierarchy string identifiers (matches `toolActions.ts`). */
export const MAX_HIERARCHY_CALL_ID_LENGTH = 512;
export const MAX_HIERARCHY_NAME_LENGTH = 2048;
