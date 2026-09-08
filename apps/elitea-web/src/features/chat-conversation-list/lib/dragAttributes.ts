/**
 * Re-roles the ARIA bag `@dnd-kit`'s `useDraggable`/`useSortable` put on their
 * draggable node.
 *
 * dnd-kit defaults that bag to `role: 'button'` (plus `tabIndex: 0`,
 * `aria-disabled`, `aria-pressed`, `aria-roledescription: 'sortable'`,
 * `aria-describedby`). That is right for a dedicated drag HANDLE. Both call
 * sites here spread it onto the row CONTAINER instead — a container wrapping
 * the conversation/folder row's own buttons and menus — so every row claimed
 * to be a single button with focusable descendants inside it. axe reports that
 * as `nested-interactive`, impact "serious", once per row (15 nodes on a list
 * with a handful of folders), and it is a real barrier: assistive tech cannot
 * reliably reach the inner controls of a widget that says it is one button.
 *
 * The fix is a role swap, NOT a strip. `group` is not a widget role, so
 * focusable descendants are allowed and `nested-interactive` does not apply,
 * while `tabIndex`, `aria-roledescription` and `aria-describedby` all stay —
 * which means dnd-kit's KeyboardSensor keeps working exactly as before (it
 * activates from a keydown on the focused activator node; the node's `role`
 * was never what made that work). Keyboard reordering and the screen-reader
 * drag instructions both survive.
 *
 * Two attributes are dropped, both for the same reason: they are WIDGET
 * STATES, and this node is no longer a widget.
 *
 * `aria-pressed` is a toggle-button state and is not allowed on
 * `role="group"` (axe `aria-allowed-attr`). dnd-kit only ever sets it to
 * `undefined` for a non-toggle draggable anyway.
 *
 * `aria-disabled` is dnd-kit's report of whether THE DRAG is available, and
 * on a container it silently disables everything inside it. Disabled state
 * INHERITS: a control inside a subtree marked `aria-disabled="true"` is
 * disabled to assistive technology, and Playwright's own `toBeEnabled`
 * resolves it the same way by walking ancestors. So a pinned conversation —
 * pinned rows are the ones that cannot be dragged into a folder
 * (`Conversations.renderers.tsx`) — rendered its whole row menu as disabled.
 * The ⋮ trigger was announced as disabled and could not be clicked, which
 * left Unpin, Rename, Delete and Share unreachable on exactly the rows a user
 * pins because they matter most. Nothing showed it: the row still looked
 * ordinary, and the menu simply never opened.
 *
 * Dropping it costs nothing dnd-kit needs. The library reads the `disabled`
 * OPTION passed to `useDraggable`/`useSortable`, never this attribute, so
 * both the pointer and the keyboard sensor keep refusing a disabled drag
 * exactly as before.
 */

/** The keys this rewrites or removes from dnd-kit's `DraggableAttributes`. */
type DragAriaKeys = 'role' | 'aria-pressed' | 'aria-disabled';

export function asDragGroupAria<T extends Partial<Record<DragAriaKeys, unknown>>>(attributes: T): Omit<T, DragAriaKeys> & { readonly role: 'group' } {
  const { role: _role, 'aria-pressed': _pressed, 'aria-disabled': _disabled, ...rest } = attributes;
  return { ...rest, role: 'group' };
}
