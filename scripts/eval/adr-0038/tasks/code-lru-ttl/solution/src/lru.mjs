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

  _expired(entry, now = this.now()) {
    return now >= entry.expiresAt;
  }

  _purgeExpired() {
    const now = this.now();
    for (const [key, entry] of this.map) {
      if (this._expired(entry, now)) this.map.delete(key);
    }
  }

  /** Returns the live entry for key, dropping it if expired. */
  _live(key) {
    const entry = this.map.get(key);
    if (!entry) return undefined;
    if (this._expired(entry)) {
      this.map.delete(key);
      return undefined;
    }
    return entry;
  }

  get(key) {
    const entry = this._live(key);
    if (!entry) return undefined;
    // Move to most-recently-used without touching expiresAt.
    this.map.delete(key);
    this.map.set(key, entry);
    return entry.value;
  }

  has(key) {
    return this._live(key) !== undefined;
  }

  set(key, value, { ttlMs = this.ttlMs } = {}) {
    const expiresAt = ttlMs ? this.now() + ttlMs : Infinity;
    this.map.delete(key);
    this.map.set(key, { value, expiresAt });
    if (this.map.size > this.max) {
      this._purgeExpired();
      while (this.map.size > this.max) {
        this.map.delete(this.map.keys().next().value);
      }
    }
    return this;
  }

  delete(key) {
    return this.map.delete(key);
  }

  get size() {
    this._purgeExpired();
    return this.map.size;
  }

  /** Live keys, most recently used first. */
  keys() {
    this._purgeExpired();
    return [...this.map.keys()].reverse();
  }
}
