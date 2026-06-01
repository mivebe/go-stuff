// game.js
//
// Config + settlement. Side-effect free: given a config, a bet, and a roll,
// it returns the same result every time. That purity is what lets us simulate
// and verify a game's RTP before it ever ships against real player money.

import { Amount } from "./asset.js";
import { ROLL_SPACE } from "./rng.js";

export class GameConfig {
  /** @param {{
   *    name: string,
   *    asset: import("./asset.js").Asset,
   *    houseEdgeBps: bigint,
   *    minBet: Amount,
   *    maxBet: Amount,
   *  }} spec */
  constructor({ name, asset, houseEdgeBps, minBet, maxBet }) {
    if (typeof houseEdgeBps !== "bigint") {
      throw new TypeError("houseEdgeBps must be BigInt");
    }
    if (minBet.asset.key !== asset.key || maxBet.asset.key !== asset.key) {
      throw new Error("minBet/maxBet must use the configured asset");
    }
    this.name = name;
    this.asset = asset;
    this.houseEdgeBps = houseEdgeBps;
    this.minBet = minBet;
    this.maxBet = maxBet;
    Object.freeze(this);
  }

  validateBet(bet) {
    if (bet.stake.asset.key !== this.asset.key) {
      throw new Error(
        `bet asset ${bet.stake.asset.key} does not match game asset ${this.asset.key}`,
      );
    }
    if (bet.stake.lt(this.minBet)) throw new Error("stake below minimum");
    if (this.maxBet.lt(bet.stake)) throw new Error("stake above maximum");
    if (!Number.isInteger(bet.target) || bet.target < 1 || bet.target >= ROLL_SPACE) {
      throw new Error("target out of range");
    }
  }

  // payoutMultiplier_bps so that RTP == (1 - houseEdge), independent of the
  // player's target:
  //   winProb    = (ROLL_SPACE - target) / ROLL_SPACE
  //   multiplier = (1 - houseEdge) / winProb
  //   RTP        = winProb × multiplier = (1 - houseEdge)
  multiplierBps(target) {
    const winOutcomes = BigInt(ROLL_SPACE - target);
    return ((10_000n - this.houseEdgeBps) * BigInt(ROLL_SPACE)) / winOutcomes;
  }

  /** Pure: apply a roll to a bet, return outcome + payout. No I/O. */
  settle(bet, roll) {
    const won = roll >= bet.target;
    const mult = this.multiplierBps(bet.target);
    return {
      roll,
      won,
      multiplierBps: mult,
      payout: won ? bet.stake.mulBps(mult) : new Amount(bet.stake.asset, 0n),
    };
  }
}
