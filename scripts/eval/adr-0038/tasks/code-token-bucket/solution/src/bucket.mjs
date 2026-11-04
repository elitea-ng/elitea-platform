// Token-bucket rate limiter with an injectable clock (milliseconds).

const positiveFinite = (v) => typeof v === 'number' && Number.isFinite(v) && v > 0;

export class TokenBucket {
  /**
   * @param {{capacity:number, refillPerSecond:number, now?:() => number}} options
   */
  constructor({ capacity, refillPerSecond, now = () => Date.now() }) {
    if (!positiveFinite(capacity)) throw new RangeError('capacity must be a positive finite number');
    if (!positiveFinite(refillPerSecond)) {
      throw new RangeError('refillPerSecond must be a positive finite number');
    }
    this.capacity = capacity;
    this.refillPerSecond = refillPerSecond;
    this.now = now;
    this.tokens = capacity;
    this.last = now();
  }

  _refill() {
    const t = this.now();
    const elapsedMs = t - this.last;
    if (elapsedMs > 0) {
      this.tokens = Math.min(this.capacity, this.tokens + (elapsedMs / 1000) * this.refillPerSecond);
    }
    // A clock that went backwards must not lead to a later double refill.
    this.last = Math.max(this.last, t);
  }

  /** Take n tokens if available. */
  tryRemove(n = 1) {
    this._refill();
    if (this.tokens >= n) {
      this.tokens -= n;
      return true;
    }
    return false;
  }

  /** Whole tokens available right now. */
  available() {
    this._refill();
    return Math.floor(this.tokens);
  }

  /** Milliseconds until n tokens are available. */
  msUntil(n = 1) {
    if (n > this.capacity) return Infinity;
    this._refill();
    const missing = n - this.tokens;
    if (missing <= 0) return 0;
    return Math.ceil((missing / this.refillPerSecond) * 1000);
  }
}
