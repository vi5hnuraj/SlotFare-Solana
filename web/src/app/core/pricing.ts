/**
 * Credits per token, by class. These three numbers are the only place
 * pricing is expressed: every figure the console shows is derived from
 * them, so a rate change never leaves a stale number on a screen.
 *
 * 1,000 / 3,000 / 100 credits per million tokens.
 */
export const RATE = {
  prompt: 0.001,
  completion: 0.003,
  cached: 0.0001,
} as const;

export type TokenClass = keyof typeof RATE;

/** 1,000 credits = $1.00. */
export const CREDITS_PER_USD = 1000;

const nf = new Intl.NumberFormat('en-US');
const nf2 = new Intl.NumberFormat('en-US', {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});
const nfCredit = new Intl.NumberFormat('en-US', {
  maximumFractionDigits: 4,
});
const nfCredit1 = new Intl.NumberFormat('en-US', {
  minimumFractionDigits: 1,
  maximumFractionDigits: 1,
});

export const num = (v: number): string => nf.format(v);
export const dec2 = (v: number): string => nf2.format(v);

/**
 * A credit amount: up to 4 decimal places (the wire's micro-credit
 * resolution), trailing zeros trimmed, whole numbers unchanged. The one
 * place credit figures are rendered; 0.0005 stays visible, never 0.
 */
export const creditText = (v: number): string => nfCredit.format(v);

/**
 * A credit balance, for a glanceable figure: exactly one decimal, rounded
 * DOWN, never up, so a balance display can never overstate what the account
 * can spend. Whole numbers keep their trailing ".0"; the +1e-6 guards
 * IEEE-754 multiplication drift without ever lifting a true floor, because
 * the wire's resolution is 0.0001 and the epsilon is far smaller.
 */
export const creditText1 = (v: number): string =>
  nfCredit1.format(Math.floor(v * 10 + 1e-6) / 10);

/** Credits rendered as the dollars they are worth. */
export const usd = (credits: number): string =>
  '$' + nf2.format(credits / CREDITS_PER_USD);

/** Up to 6 decimals, trailing zeros trimmed: the swap input side's figure.
 *  Small ETH amounts like 0.001 or 0.00101 must never truncate to "0.00",
 *  so the swap review and the funding trace's Paid line both format through
 *  this one rule. */
const nfSwap = new Intl.NumberFormat('en-US', { maximumFractionDigits: 6 });

/** A swap input amount in base units, as human units through `nfSwap`. */
export const swapAmountText = (baseUnits: number, decimals: number): string =>
  nfSwap.format(baseUnits / 10 ** decimals);

/** How many millions of tokens of one class a credit balance buys. */
export function mTokens(credits: number, rate: number): string {
  const t = credits / rate / 1e6;
  return (t >= 100 ? Math.round(t) : t.toFixed(1)) + 'M';
}

/** mm:ss, for elapsed and remaining time on the funding trace. */
export function clock(seconds: number): string {
  const m = Math.floor(seconds / 60);
  const s = Math.floor(seconds % 60);
  return `${m < 10 ? '0' : ''}${m}:${s < 10 ? '0' : ''}${s}`;
}

/** What one request costs, in credits. Cached tokens are not also prompt. */
export function costOf(u: {
  prompt: number;
  cached: number;
  completion: number;
}): number {
  return (
    u.prompt * RATE.prompt +
    u.cached * RATE.cached +
    u.completion * RATE.completion
  );
}
