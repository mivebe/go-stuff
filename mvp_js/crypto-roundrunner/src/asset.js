// asset.js
//
// Money in a crypto product is NEVER a Number.
//
// - 1 ETH in base units (wei) is 10^18, which is greater than
//   Number.MAX_SAFE_INTEGER (2^53 - 1 ≈ 9 × 10^15). Using a JS Number for wei
//   silently loses precision and creates ledger drift you can't reconstruct.
// - Even 1 BTC in sats (10^8) is "safe" today but doing fee math in sats
//   gets you into ranges where products overflow Number long before BigInt does.
// - JS only allows BigInt-with-BigInt arithmetic; mixing with Number throws.
//   That's actually a feature: it forces every code path to be explicit.
//
// Every amount is therefore a BigInt count of an asset's smallest indivisible
// unit (wei, sat, micro-USDT, ...), tagged with the Asset it belongs to so
// the runtime can reject "BTC + USDT" as the bug it is.

export class Asset {
  /** @param {{symbol: string, chain: string, decimals: number}} spec */
  constructor({ symbol, chain, decimals }) {
    if (!Number.isInteger(decimals) || decimals < 0 || decimals > 30) {
      throw new RangeError(`decimals must be a non-negative integer ≤ 30, got ${decimals}`);
    }
    this.symbol = symbol;
    this.chain = chain;
    this.decimals = decimals;
    Object.freeze(this);
  }

  // Identity = (symbol, chain). "USDT on Tron" and "USDT on Ethereum" are
  // DIFFERENT assets even though the symbol matches.
  get key() {
    return `${this.symbol}:${this.chain}`;
  }

  // Render base units for humans, trimming trailing zeros.
  format(value) {
    if (typeof value !== "bigint") {
      throw new TypeError(`format requires a BigInt, got ${typeof value}`);
    }
    if (this.decimals === 0) return `${value} ${this.symbol}`;
    const sign = value < 0n ? "-" : "";
    const abs = value < 0n ? -value : value;
    const div = 10n ** BigInt(this.decimals);
    const whole = abs / div;
    const fracStr = (abs % div).toString().padStart(this.decimals, "0").replace(/0+$/, "");
    return `${sign}${whole}.${fracStr || "0"} ${this.symbol}`;
  }
}

// Well-known assets. Notice the wildly different decimals — that is exactly
// the asset-mismatch class of bug you protect against by typing amounts.
export const BTC = new Asset({ symbol: "BTC", chain: "bitcoin", decimals: 8 });
export const ETH = new Asset({ symbol: "ETH", chain: "ethereum", decimals: 18 });
export const USDT_TRON = new Asset({ symbol: "USDT", chain: "tron", decimals: 6 });
export const USDT_ETH = new Asset({ symbol: "USDT", chain: "ethereum", decimals: 6 });
export const USDC_ETH = new Asset({ symbol: "USDC", chain: "ethereum", decimals: 6 });

const ASSETS_BY_KEY = Object.fromEntries(
  [BTC, ETH, USDT_TRON, USDT_ETH, USDC_ETH].map((a) => [a.key, a]),
);
export function lookupAsset(key) {
  const a = ASSETS_BY_KEY[key];
  if (!a) throw new Error(`unknown asset: ${key}`);
  return a;
}

// Amount is a (asset, bigint) pair. All arithmetic enforces asset equality.
// The class is frozen — mutation goes through new instances.
export class Amount {
  /** @param {Asset} asset @param {bigint} value */
  constructor(asset, value) {
    if (!(asset instanceof Asset)) throw new TypeError("asset must be an Asset");
    if (typeof value !== "bigint") {
      throw new TypeError(`Amount value must be BigInt, got ${typeof value}`);
    }
    this.asset = asset;
    this.value = value;
    Object.freeze(this);
  }

  add(other) {
    this.#requireSameAsset(other);
    return new Amount(this.asset, this.value + other.value);
  }
  sub(other) {
    this.#requireSameAsset(other);
    return new Amount(this.asset, this.value - other.value);
  }
  neg() {
    return new Amount(this.asset, -this.value);
  }
  isZero() {
    return this.value === 0n;
  }
  isNegative() {
    return this.value < 0n;
  }
  lt(other) {
    this.#requireSameAsset(other);
    return this.value < other.value;
  }

  // Multiply by basis points (1 bps = 1/10000). Used to apply a payout
  // multiplier while keeping the math in integers. Stakes are non-negative
  // here, so BigInt's truncate-toward-zero division equals floor — which is
  // the house-favourable rounding direction. Rounding policy must be EXPLICIT.
  mulBps(bps) {
    const bpsN = typeof bps === "bigint" ? bps : BigInt(bps);
    if (this.value < 0n) {
      // Not used today, but the rule must be defended: don't paper over a
      // negative input by accident.
      throw new RangeError("mulBps on a negative amount: define your rounding first");
    }
    return new Amount(this.asset, (this.value * bpsN) / 10_000n);
  }

  // Stable serialisation. Critical: BigInt has no JSON representation, so we
  // emit a decimal string. The ledger hash depends on this being stable.
  toJSON() {
    return { asset: this.asset.key, v: this.value.toString() };
  }
  static fromJSON(obj) {
    return new Amount(lookupAsset(obj.asset), BigInt(obj.v));
  }

  toString() {
    return this.asset.format(this.value);
  }

  #requireSameAsset(other) {
    if (!(other instanceof Amount)) throw new TypeError("expected Amount");
    if (this.asset.key !== other.asset.key) {
      throw new TypeError(
        `asset mismatch: ${this.asset.key} vs ${other.asset.key}`,
      );
    }
  }
}
