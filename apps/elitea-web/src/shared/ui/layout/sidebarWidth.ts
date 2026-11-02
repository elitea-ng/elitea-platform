/**
 * The one definition of the app chrome's rail width.
 *
 * There used to be two, and they disagreed:
 *
 *   widgets/sidebar/ui/Sidebar.tsx   208px / 72px
 *   pages/admin/AdminNav.tsx         220px / 60px  ("13.75rem"/"3.75rem")
 *
 * The main app's rail and the admin rail sit in the same product, and their
 * ITEM styling is already byte-comparable — same radius, same 2rem height,
 * same `drawerMenu.hover|selected` tokens. Only the outer width differed,
 * which is exactly the kind of divergence that survives review: nothing looks
 * broken on either screen on its own, and you only see it switching between
 * them.
 *
 * Neither number was right. The baseline states both
 * (`apps/elitea-ui/src/common/constants.js:51,53`):
 *
 *   SIDE_BAR_WIDTH = 216            COLLAPSED_SIDE_BAR_WIDTH = 64
 *
 * so this is not a matter of picking a winner between the two forks — both
 * had drifted, in opposite directions, on both values.
 *
 * The expanded width has since grown past the baseline's (see below); both
 * rails still read this one number.
 *
 * Exported in px AND rem because the two call sites want different units:
 * `Sidebar.tsx` computes a `calc()` against a pixel width, `AdminNav.tsx`
 * writes a rem string straight into `sx`. Same number either way.
 */

/**
 * Wider than the baseline's 216: the rail now carries the chat list (the
 * shared `widgets/chat-rail`, the desktop sidebar's layout), whose titles and
 * indented Workshop rows ("Toolkits & Indexes") truncated at 216.
 */
export const SIDE_BAR_WIDTH_PX = 248;

/** Baseline `COLLAPSED_SIDE_BAR_WIDTH` (`common/constants.js:53`). */
export const COLLAPSED_SIDE_BAR_WIDTH_PX = 64;

/** `SIDE_BAR_WIDTH_PX` as rem at the 16px root — `15.5rem`. */
export const SIDE_BAR_WIDTH_REM = `${SIDE_BAR_WIDTH_PX / 16}rem`;

/** `COLLAPSED_SIDE_BAR_WIDTH_PX` as rem at the 16px root — `4rem`. */
export const COLLAPSED_SIDE_BAR_WIDTH_REM = `${COLLAPSED_SIDE_BAR_WIDTH_PX / 16}rem`;
