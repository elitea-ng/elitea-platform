/**
 * The shell's right panel, for a page: the desktop inspector or the web side
 * panel, whichever the build has (`shared/desktop/inspectorSlot`). A page puts
 * what belongs beside its content — a list's Filters, the chat's participants
 * — in a `<SidePanelTab>`, and opens it with `useSidePanelSlot()?.show(id)`.
 */
export { InspectorTab as SidePanelTab, useSidePanelSlot } from '@/shared/desktop/inspectorSlot';
