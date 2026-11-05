// Shopping cart. Prices are integer cents.

export function createCart() {
  return { items: [] };
}

/**
 * @param {{items: object[]}} cart
 * @param {{sku: string, name: string, priceCents: number, qty?: number}} item
 */
export function addItem(cart, { sku, name, priceCents, qty = 1 }) {
  if (!Number.isSafeInteger(priceCents) || priceCents < 0) {
    throw new RangeError(`priceCents must be a non-negative integer, got ${priceCents}`);
  }
  if (!Number.isSafeInteger(qty) || qty < 1) {
    throw new RangeError(`qty must be a positive integer, got ${qty}`);
  }
  const existing = cart.items.find((i) => i.sku === sku);
  if (existing) existing.qty += qty;
  else cart.items.push({ sku, name, priceCents, qty });
  return cart;
}

export function lineTotalCents(item) {
  return item.priceCents * item.qty;
}

export function subtotalCents(cart) {
  return cart.items.reduce((sum, item) => sum + lineTotalCents(item), 0);
}
