// Shopping cart. Prices are dollars as floating-point numbers.

export function createCart() {
  return { items: [] };
}

/**
 * @param {{items: object[]}} cart
 * @param {{sku: string, name: string, price: number, qty?: number}} item
 */
export function addItem(cart, { sku, name, price, qty = 1 }) {
  if (price < 0) throw new RangeError('price must not be negative');
  const existing = cart.items.find((i) => i.sku === sku);
  if (existing) existing.qty += qty;
  else cart.items.push({ sku, name, price, qty });
  return cart;
}

export function lineTotal(item) {
  return item.price * item.qty;
}

export function subtotal(cart) {
  return cart.items.reduce((sum, item) => sum + lineTotal(item), 0);
}
