import { dec2 } from './pricing';

/**
 * One listing or market price, as the console displays it: the raw wire
 * amount scaled down by the pay token's decimals, next to the token's
 * symbol (or a shortened address). The rows keep `price` in raw units;
 * the scaling happens here and only here.
 */

/** 0x1234...abcd for a 0x address with at least eight hex chars, and
 *  AbCd...wXyZ for a base58 wallet of at least eight chars, else ''.
 *  A cell never shows a bare truncated fragment. */
export function shortAddress(address: string | undefined): string {
  if (typeof address !== 'string' || address.length < 8) return '';
  const hex = /^0x([0-9a-fA-F]{8,})$/.exec(address);
  if (hex !== null) return `0x${hex[1].slice(0, 4)}...${hex[1].slice(-4)}`;
  if (/^[1-9A-HJ-NP-Za-km-z]{8,}$/.test(address)) {
    return `${address.slice(0, 4)}...${address.slice(-4)}`;
  }
  return '';
}

/**
 * A usable pay-token symbol from the wire. The wire field sometimes carries
 * a shortened address instead of a symbol (the bug-era rows), so an empty
 * value or anything 0x-prefixed is treated as absent and yields '', which
 * the caller resolves by address. A 0x-prefixed "symbol" is an address
 * fragment, never a symbol.
 */
export function usableSymbol(symbol?: string): string {
  if (typeof symbol !== 'string') return '';
  const s = symbol.trim();
  if (s === '' || s.startsWith('0x')) return '';
  return s;
}

export interface ListingPriceParts {
  /** The price formatted to two decimals: scaled when the token's decimals
   *  are known, or the raw figure with an explicit ` raw` marker when
   *  they are not, rather than a silently wrong number. */
  text: string;
  /** The pay token's symbol, else its shortened address, else the
   *  `-` placeholder the rest of the console uses for empty values. */
  label: string;
}

/** The pay token's decimals: the wire's figure when it is a finite number
 *  strictly greater than zero. The server's wire contract defines 0 as
 *  unknown, never a real zero-decimal token; else null. The wire is the
 *  only source: a row without decimals shows the raw marker honestly. */
function payTokenDecimals(row: { tokenAddress?: string; tokenDecimals?: number }): number | null {
  const w = row.tokenDecimals;
  if (typeof w === 'number' && Number.isFinite(w) && w > 0) return w;
  return null;
}

/**
 * Format one listing or market price for display. `row.price` is the raw
 * wire amount; the result is what every price cell shows. A 0x-prefixed
 * symbol is treated as absent and falls back to the shortened token
 * address; without decimals the cell shows the raw marker instead of a
 * silently wrong number.
 */
export function listingPrice(row: {
  price: number;
  token?: string;
  tokenAddress?: string;
  tokenDecimals?: number;
}): ListingPriceParts {
  const decimals = payTokenDecimals(row);
  const short = shortAddress(row.tokenAddress);
  const symbol = usableSymbol(row.token);
  const label = symbol !== '' ? symbol : short !== '' ? short : '-';
  const text = decimals === null ? `${dec2(row.price)} raw` : dec2(row.price / 10 ** decimals);
  return { text, label };
}
