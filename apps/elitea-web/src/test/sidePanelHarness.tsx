/**
 * A shell side panel for page tests: every tab a page registers through
 * `shared/ui/side-panel` (`<SidePanelTab>`) renders, at once, in its own
 * `side-panel-tab-<id>` box beside the page — the way the desktop inspector or
 * the web side panel would show it, without the shell around the page.
 * `onShow` hears the tabs the page asks the panel to open (the toolbar's
 * Filters button).
 */
import { useCallback, useMemo, useRef, useState, type ReactNode } from 'react';

import { SidePanelSlotProvider, type InspectorTabId, type InspectorTabInfo, type InspectorTarget, type SidePanelSlot } from '@/shared/desktop/inspectorSlot';

export function SidePanelHarness({ children, onShow }: { readonly children: ReactNode; readonly onShow?: (tab: InspectorTabId) => void }): ReactNode {
  const [tabs, setTabs] = useState<readonly InspectorTabInfo[]>([]);
  const [nodes, setNodes] = useState<ReadonlyMap<InspectorTabId, HTMLElement>>(new Map());

  const register = useCallback((info: InspectorTabInfo) => {
    setTabs((current) => [...current.filter((tab) => tab.id !== info.id), info]);
    return () => setTabs((current) => current.filter((tab) => tab !== info));
  }, []);
  const setNode = useCallback((id: InspectorTabId, node: HTMLElement | null) => {
    setNodes((current) => {
      if (node === null ? !current.has(id) : current.get(id) === node) return current;
      const next = new Map(current);
      if (node === null) next.delete(id);
      else next.set(id, node);
      return next;
    });
  }, []);

  // One ref callback per tab for the harness's lifetime: a fresh one per render would detach and reattach the box every render.
  const refs = useRef(new Map<InspectorTabId, (node: HTMLElement | null) => void>());
  const refFor = (id: InspectorTabId): ((node: HTMLElement | null) => void) => {
    let ref = refs.current.get(id);
    if (ref === undefined) {
      ref = (node) => setNode(id, node);
      refs.current.set(id, ref);
    }
    return ref;
  };

  const slot = useMemo<SidePanelSlot>(() => {
    const targets = new Map<InspectorTabId, InspectorTarget>();
    nodes.forEach((node, id) => targets.set(id, { node }));
    return { register, targets, show: (tab) => onShow?.(tab) };
  }, [register, nodes, onShow]);

  return (
    <SidePanelSlotProvider value={slot}>
      {children}
      <aside data-testid="side-panel-harness">
        {tabs.map((info) => (
          <div
            key={info.id}
            aria-label={info.title}
            data-testid={`side-panel-tab-${info.id}`}
            ref={refFor(info.id)}
          />
        ))}
      </aside>
    </SidePanelSlotProvider>
  );
}
