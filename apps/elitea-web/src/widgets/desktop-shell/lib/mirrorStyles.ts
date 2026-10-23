/**
 * Keeps a panel window's document styled like the main window's: every
 * stylesheet of `source` copied into `target` (rule by rule, so the rules
 * emotion inserts through the CSSOM — invisible to a MutationObserver — are
 * copied too) and the root element's attributes (the colour scheme MUI sets
 * on `<html>`) mirrored.
 *
 * `sync()` is cheap when nothing changed (it compares rule counts); the
 * caller runs it when `watchStyles` says the source changed. Emotion only
 * ever appends rules; a sheet that shrank is copied again from scratch. A
 * sheet whose rules cannot be read (a cross-origin stylesheet) is copied as
 * its `<link>`. The copies sit in the target's head in the source's sheet
 * order, so the cascade between them is the source's.
 */

interface Copy {
  readonly node: HTMLElement;
  copied: number;
}

export interface StyleMirror {
  readonly sync: () => void;
  readonly dispose: () => void;
}

function readRules(sheet: CSSStyleSheet): CSSRuleList | null {
  try {
    return sheet.cssRules;
  } catch {
    return null;
  }
}

function mirrorAttributes(from: Element, to: Element): void {
  for (const attribute of Array.from(to.attributes)) {
    if (!from.hasAttribute(attribute.name)) to.removeAttribute(attribute.name);
  }
  for (const attribute of Array.from(from.attributes)) {
    if (to.getAttribute(attribute.name) !== attribute.value) to.setAttribute(attribute.name, attribute.value);
  }
}

export function mirrorStyles(source: Document, target: Document): StyleMirror {
  const copies = new Map<CSSStyleSheet, Copy>();

  /** The copy of the first sheet after `at` (in the source's order) that has one: where a new copy goes, before it. */
  const nextCopy = (sheets: readonly CSSStyleSheet[], at: number): HTMLElement | null => {
    for (let index = at + 1; index < sheets.length; index += 1) {
      const copy = copies.get(sheets[index] as CSSStyleSheet);
      if (copy !== undefined && copy.node.parentNode === target.head) return copy.node;
    }
    return null;
  };

  const copyOf = (sheets: readonly CSSStyleSheet[], at: number, rules: CSSRuleList | null): Copy | undefined => {
    const sheet = sheets[at] as CSSStyleSheet;
    const existing = copies.get(sheet);
    if (existing !== undefined && (rules === null || rules.length >= existing.copied)) return existing;
    existing?.node.remove();
    let node: HTMLElement;
    if (rules === null) {
      if (!(sheet.ownerNode instanceof Element) || sheet.ownerNode.tagName !== 'LINK') return undefined;
      node = target.importNode(sheet.ownerNode, false) as HTMLElement;
    } else {
      node = target.createElement('style');
      if (sheet.media.mediaText !== '') node.setAttribute('media', sheet.media.mediaText);
    }
    node.dataset['mirror'] = '';
    target.head.insertBefore(node, nextCopy(sheets, at));
    const copy = { node, copied: 0 };
    copies.set(sheet, copy);
    return copy;
  };

  const sync = (): void => {
    mirrorAttributes(source.documentElement, target.documentElement);
    const live = new Set<CSSStyleSheet>();
    const sheets = Array.from(source.styleSheets);
    sheets.forEach((sheet, at) => {
      live.add(sheet);
      const rules = readRules(sheet);
      const copy = copyOf(sheets, at, rules);
      if (copy === undefined || rules === null || rules.length === copy.copied) return;
      const into = (copy.node as HTMLStyleElement).sheet;
      if (into === null) return;
      for (let index = copy.copied; index < rules.length; index += 1) {
        const rule = rules[index];
        try {
          if (rule !== undefined) into.insertRule(rule.cssText, into.cssRules.length);
        } catch {
          // A rule the target's engine refuses (a vendor prefix it does not know): skipped, as the source skipped it.
        }
      }
      copy.copied = rules.length;
    });
    for (const [sheet, copy] of copies) {
      if (live.has(sheet)) continue;
      copy.node.remove();
      copies.delete(sheet);
    }
  };

  return {
    sync,
    dispose: () => {
      copies.forEach((copy) => copy.node.remove());
      copies.clear();
    },
  };
}

/**
 * Calls `onChange` (at most once per microtask, so before the next paint)
 * whenever `source`'s styles may have changed: a node in its head added,
 * removed or edited, an attribute of its root element (the colour scheme)
 * changed, or a rule inserted through the CSSOM — which no MutationObserver
 * sees, and is how emotion adds rules in a production build. For the last,
 * `insertRule` of the source window's `CSSStyleSheet` is wrapped while the
 * watch lasts.
 */
export function watchStyles(source: Document, onChange: () => void): () => void {
  const win = source.defaultView;
  let queued = false;
  const schedule = (): void => {
    if (queued) return;
    queued = true;
    queueMicrotask(() => {
      queued = false;
      onChange();
    });
  };
  const observer = new MutationObserver(schedule);
  observer.observe(source.head, { childList: true, subtree: true, characterData: true });
  observer.observe(source.documentElement, { attributes: true });

  const proto = win?.CSSStyleSheet.prototype;
  const descriptor = proto === undefined ? undefined : Object.getOwnPropertyDescriptor(proto, 'insertRule');
  const original = descriptor?.value as ((this: CSSStyleSheet, rule: string, index?: number) => number) | undefined;
  if (proto !== undefined && original !== undefined) {
    proto.insertRule = function insertRule(this: CSSStyleSheet, rule: string, index?: number): number {
      const at = original.call(this, rule, index);
      if (this.ownerNode?.ownerDocument === source) schedule();
      return at;
    };
  }
  return () => {
    observer.disconnect();
    if (proto !== undefined && descriptor !== undefined) Object.defineProperty(proto, 'insertRule', descriptor);
  };
}
