// Token-bucket rate limiter with an injectable clock (milliseconds).

export class TokenBucket {
  /**
   * @param {{capacity:number, refillPerSecond:number, now?:() => number}} options
   */
  constructor({ capacity, refillPerSecond, now = () => Date.now() }) {
    if (!(capacity > 0)) throw new RangeError('capacity must be positive');
    this.capacity = capacity;
    this.refillPerSecond = refillPerSecond;
    this.now = now;
    this.tokens = capacity;
    this.last = now();
  }

  _refill() {
    const t = this.now();
    const elapsedSeconds = Math.floor((t - this.last) / 1000);
    this.tokens = this.tokens + elapsedSeconds * this.refillPerSecond;
    this.last = t;
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
    return Math.round(this.tokens);
  }

  /** Milliseconds until n tokens are available. */
  msUntil(n = 1) {
    const missing = n - this.tokens;
    if (missing <= 0) return 0;
    return missing / this.refillPerSecond;
  }
}
