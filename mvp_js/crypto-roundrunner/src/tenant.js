// tenant.js
//
// The platform is a GAME PROVIDER (an RGS), not the casino. Its tenants are
// OPERATORS — the licensed casino brands that own the player relationship and
// the wallet. This module is the tenant boundary: per-operator config and the
// registry that resolves an operatorId to its config + wallet client, or
// rejects an unknown operator outright.
//
// Everything operator-specific (enabled games, allowed assets, bet limits,
// callback secret, wallet endpoint dialect) lives here at the edge; the engine
// core stays operator-agnostic. See PLATFORM_PRIMER.md §3.

export class UnknownOperatorError extends Error {
  constructor(operatorId) {
    super(`unknown operator: ${operatorId}`);
    this.name = "UnknownOperatorError";
    this.operatorId = operatorId;
  }
}

export class TenantConfigError extends Error {
  constructor(message) {
    super(message);
    this.name = "TenantConfigError";
  }
}

// Per-operator configuration. Frozen: changing a tenant means registering a new
// config, not mutating a live one — config rotation is a registry operation.
export class TenantConfig {
  /** @param {{
   *    operatorId: string,
   *    displayName?: string,
   *    enabledGames: string[],          // game names this operator may run
   *    allowedAssets: string[],         // Asset.key values this operator may use
   *    limits?: Record<string, {min: bigint, max: bigint}>, // per assetKey override
   *    callbackSecret: string,          // per-operator HMAC key for seamless calls
   *    jurisdiction?: string,
   *  }} spec */
  constructor({
    operatorId,
    displayName,
    enabledGames,
    allowedAssets,
    limits = {},
    callbackSecret,
    jurisdiction = "unspecified",
  }) {
    if (!operatorId) throw new TenantConfigError("operatorId required");
    if (!Array.isArray(enabledGames) || enabledGames.length === 0) {
      throw new TenantConfigError("enabledGames must be a non-empty array");
    }
    if (!Array.isArray(allowedAssets) || allowedAssets.length === 0) {
      throw new TenantConfigError("allowedAssets must be a non-empty array");
    }
    // A per-operator secret is non-negotiable: one operator's key must never
    // validate another's seamless traffic (EDGE_CASES.md §13).
    if (!callbackSecret) throw new TenantConfigError("callbackSecret required");

    for (const [k, lim] of Object.entries(limits)) {
      if (typeof lim.min !== "bigint" || typeof lim.max !== "bigint") {
        throw new TenantConfigError(`limits.${k} min/max must be BigInt`);
      }
    }

    this.operatorId = operatorId;
    this.displayName = displayName ?? operatorId;
    this.enabledGames = new Set(enabledGames);
    this.allowedAssets = new Set(allowedAssets);
    this.limits = limits;
    this.callbackSecret = callbackSecret;
    this.jurisdiction = jurisdiction;
    Object.freeze(this);
  }

  allowsGame(gameName) {
    return this.enabledGames.has(gameName);
  }

  allowsAsset(asset) {
    return this.allowedAssets.has(asset.key);
  }

  /** Per-operator stake limit for an asset, if this tenant overrides the
   *  game's own min/max. Returns null when the game default applies. */
  limitFor(asset) {
    return this.limits[asset.key] ?? null;
  }
}

// OperatorRegistry — the one abstraction the engine depends on for tenancy.
// Maps operatorId -> { config, wallet }. The wallet is that operator's seamless
// wallet client (its endpoint + credentials baked in). Resolving an unknown
// operator throws: the isolation boundary fails closed.
export class OperatorRegistry {
  #tenants = new Map(); // operatorId -> { config, wallet }

  /** @param {TenantConfig} config @param {object} wallet seamless wallet client */
  register(config, wallet) {
    if (!(config instanceof TenantConfig)) {
      throw new TenantConfigError("config must be a TenantConfig");
    }
    if (this.#tenants.has(config.operatorId)) {
      throw new TenantConfigError(`operator already registered: ${config.operatorId}`);
    }
    if (!wallet || typeof wallet.debit !== "function") {
      throw new TenantConfigError("wallet must implement the seamless interface");
    }
    this.#tenants.set(config.operatorId, { config, wallet });
  }

  has(operatorId) {
    return this.#tenants.has(operatorId);
  }

  /** Resolve a tenant. Throws UnknownOperatorError — fail closed. */
  resolve(operatorId) {
    const t = this.#tenants.get(operatorId);
    if (!t) throw new UnknownOperatorError(operatorId);
    return t;
  }

  operatorIds() {
    return [...this.#tenants.keys()];
  }
}
