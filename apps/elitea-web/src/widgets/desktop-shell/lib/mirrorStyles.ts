/**
 * Keeps a panel window's document styled like the main window's: every
 * stylesheet of `source` copied into `target` (rule by rule, so the rules
 * emotion inserts through the CSSOM — invisible to a MutationObserver — are
 * copied too) and the root element's attributes (the colour scheme MUI sets
 * on `<html>`) mirrored.
 *
 * `sync()` is cheap when nothing changed (it compares rule counts), so the
 * caller runs it on every frame of the target window: a rule a component
 * inserted while rendering into that window is there before the window
 * paints. Emotion only ever appends rules; a sheet that shrank is copied
 * again from scratch. A sheet whose rules cannot be read (a cross-origin
 * stylesheet) is copied as its `<link>`.
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

  const copyOf = (sheet: CSSStyleSheet, rules: CSSRuleList | null): Copy | undefined => {
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
    target.head.append(node);
    const copy = { node, copied: 0 };
    copies.set(sheet, copy);
    return copy;
  };

  const sync = (): void => {
    mirrorAttributes(source.documentElement, target.documentElement);
    const live = new Set<CSSStyleSheet>();
    for (const sheet of Array.from(source.styleSheets)) {
      live.add(sheet);
      const rules = readRules(sheet);
      const copy = copyOf(sheet, rules);
      if (copy === undefined || rules === null || rules.length === copy.copied) continue;
      const into = (copy.node as HTMLStyleElement).sheet;
      if (into === null) continue;
      for (let index = copy.copied; index < rules.length; index += 1) {
        const rule = rules[index];
        try {
          if (rule !== undefined) into.insertRule(rule.cssText, into.cssRules.length);
        } catch {
          // A rule the target's engine refuses (a vendor prefix it does not know): skipped, as the source skipped it.
        }
      }
      copy.copied = rules.length;
    }
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
