/**
 * The desktop app has no web page context menu. WebKit's default one
 * ("Reload", "Inspect Element", "Back") is browser chrome, not app UI: a
 * right-click on anything else does nothing, as in a native app.
 *
 * Kept where the system menu is the native behaviour: inside an editable
 * field (Cut / Copy / Paste, spelling) and over a text selection (Copy, Look
 * Up). Holding ⌥ while right-clicking keeps the default too, so a developer
 * running a debug build can still reach the inspector.
 */
function isEditable(target: EventTarget | null): boolean {
  if (!(target instanceof Element)) return false;
  return target.closest('input, textarea, [contenteditable=""], [contenteditable="true"], [contenteditable="plaintext-only"]') !== null;
}

function hasSelection(win: Window): boolean {
  const selection = win.getSelection();
  return selection !== null && !selection.isCollapsed && selection.toString().trim() !== '';
}

export function installContextMenuGuard(doc: Document = document, win: Window = window): () => void {
  const onContextMenu = (event: MouseEvent): void => {
    if (event.defaultPrevented || event.altKey) return;
    if (isEditable(event.target) || hasSelection(win)) return;
    event.preventDefault();
  };
  doc.addEventListener('contextmenu', onContextMenu);
  return () => doc.removeEventListener('contextmenu', onContextMenu);
}
