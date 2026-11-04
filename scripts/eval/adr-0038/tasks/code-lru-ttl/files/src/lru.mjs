// LRU cache with optional per-entry TTL. The Map's insertion order is the
// recency order: first entry = least recently used.

export class LRUCache {
  /**
   * @param {{ max: number, ttlMs?: number, now?: () => number }} options
   */
  constructor({ max, ttlMs = 0, now = () => Date.now() }) {
    if (!Number.isInteger(max) || max < 1) throw new RangeError('max must be a positive integer');
    this.max = max;
    this.ttlMs = ttlMs;
    this.now = now;
    this.map = new Map();
  }

  _expired(entry) {
    return entry.expiresAt !== Infinity && this.now() > entry.expiresAt;
  }

  get(key) {
    const entry = this.map.get(key);
    if (!entry) return undefined;
    if (this._expired(entry)) {
      this.map.delete(key);
      return undefined;
    }
    this.map.delete(key);
    this.set(key, entry.value);
    return entry.value;
  }

  has(key) {
    return this.get(key) !== undefined;
  }

  set(key, value, { ttlMs = this.ttlMs } = {}) {
    const expiresAt = ttlMs ? this.now() + ttlMs : Infinity;
    this.map.set(key, { value, expiresAt });
    if (this.map.size > this.max) {
      const oldest = this.map.keys().next().value;
      this.map.delete(oldest);
    }
    return this;
  }

  delete(key) {
    return this.map.delete(key);
  }

  get size() {
    return this.map.size;
  }

  /** Live keys, most recently used first. */
  keys() {
    return [...this.map.keys()].reverse();
  }
}
