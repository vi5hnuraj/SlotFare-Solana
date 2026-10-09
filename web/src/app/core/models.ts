/**
 * One model the endpoint serves, as the console renders it. `pricing` holds
 * whole credits per million tokens, one figure per token class; the wire
 * keeps its field names and this type is the presentation vocabulary.
 */
export interface ModelPricing {
  /** Credits per million uncached input tokens. */
  uncachedInput: number;
  /** Credits per million cached input tokens. */
  cachedInput: number;
  /** Credits per million output tokens. */
  output: number;
}

/** The kind of inference a model serves; absent on the wire means chat. */
export type ModelKind = 'chat' | 'embeddings' | 'reranker';

/** The serving lane the backend routes the model down; absent means
 *  standard. Only `priority` and `fast` are badged in the UI. */
export type ServingPath = 'standard' | 'priority' | 'fast';

/** A capability flag the catalog carries for a model. */
export type ModelCapability =
  | 'text'
  | 'image'
  | 'video'
  | 'audio'
  | 'vision'
  | 'function-calling';

export interface Model {
  id: string;
  name: string;
  pricing: ModelPricing;
  /** The vendor name the wire carries; absent models group under "other". */
  vendor?: string;
  /** Context window in tokens, when the wire carries one. */
  contextLength?: number;
  /** The kind the wire advertises; absent means chat. */
  kind?: ModelKind;
  /** The serving lane the wire advertises; absent means standard. */
  servingPath?: ServingPath;
  /** One-line model description, when the wire carries one. */
  description?: string;
  /** Parameter count as the wire carries it, e.g. "27B". */
  parameters?: string;
  /** Capability flags, when the wire carries any. */
  capabilities?: ModelCapability[];
  /** Whether the wire marks the model as a mixture of experts. */
  moe?: boolean;
}

export interface ApiKey {
  id: string;
  display: string;
  label: string;
  created: string;
  /** ISO timestamp of the last request, or null when the key has never been used. */
  lastUsedAt: string | null;
  requests: number;
  revoked: boolean;
}

export interface UsageRow {
  /** Stable row id; the wire row's id. The dedupe key when older pages are
   *  merged over the newest batch. */
  id: string;
  time: string;
  key: string;
  model: string;
  prompt: number | null;
  cached: number | null;
  completion: number | null;
  credits: number;
  result: string;
  latencyMs: number | null;
  error: string | null;
  /** The settlement facts of a settled row: which batch committed it, to
   *  which root, in which transaction. Null while the charge is awaiting
   *  settlement (and on error rows, which are never charged). */
  commitment: { batchIndex: number; root: string; commitTx: string } | null;
}

export interface Wallet {
  name: string;
  rdns: string;
  initial: string;
  /** The provider's own icon (EIP-6963 `providerInfo.icon`, a data URI),
   *  when the announcement carried one. Absent means the letter avatar
   *  stands in. */
  icon?: string;
}

export interface StableToken {
  symbol: string;
  held: string;
  available: boolean;
}

/**
 * One of the account's market listings, as the console renders it. `id`
 * is the listing's on-chain numeric id; until the backend has recorded
 * the row, the listing transaction hash stands in as the id (the D57
 * pattern). The backend's row is recognised by `listedTx`, the same
 * hash the wallet returned, not by that stand-in id.
 */
export interface Listing {
  id: string;
  /** The transaction that recorded this listing on chain. For a local
   *  pending row it equals the stand-in `id`; for a server row it is how
   *  the store drops its local twin. */
  listedTx: string;
  state: 'open' | 'cancelled' | 'sold';
  /** Which side of the market this row is on, straight off the wire: both
   *  roles share `GET /console/listings`. */
  role: 'buying' | 'selling';
  /** Whole credits locked by this listing. */
  credits: number;
  /** Raw pay-token units, smallest unit; scale with `tokenDecimals` before
   *  display. The wire reports the amount the market is paid, unscaled. */
  price: number;
  /** Pay token symbol, e.g. mUSD1. */
  token: string;
  /** Pay token address; absent for local-pending rows built before the
   *  environment lookup ran, and when the wire omits it. */
  tokenAddress?: string;
  /** Pay token decimals, straight off the wire when present. */
  tokenDecimals?: number;
  /** Server boolean: the maturation window has elapsed. */
  matured: boolean;
  /** ISO timestamp the backend recorded the listing under. */
  createdAt: string;
  /** The block the listing transaction landed in. */
  listedBlock?: number;
  /** Unix seconds of the maturation deadline that gated the sale. */
  maturesAt?: number;
  /** The sale transaction (present when state is 'sold'). */
  soldTx?: string;
  /** The block the sale transaction landed in. */
  soldBlock?: number;
  /** ISO timestamp the sale settled. */
  soldAt?: string;
  /** The cancel transaction (present when state is 'cancelled'). */
  cancelledTx?: string;
  /** The block the cancel transaction landed in. */
  cancelledBlock?: number;
  /** ISO timestamp the listing was cancelled. */
  cancelledAt?: string;
}

/**
 * One open listing on the market, from any seller, as the buyer's Market
 * screen renders it. Unlike `Listing` (the account's own rows), this is a
 * read-only offer. `mine` is the server's ownership signal. Accounts are
 * hashed wallets, so no seller address travels on the wire on purpose and
 * the server is the one that knows; `mine` replaces any client-side address
 * compare. A `mine` row is never buyable, and `matured` remains the only
 * authority on maturity.
 */
export interface MarketListing {
  /** Stringified on-chain listing id, what `buy(listingId)` takes. */
  id: string;
  /** Whole credits offered for sale. */
  credits: number;
  /** Raw pay-token units, smallest unit; scale with `tokenDecimals` before
   *  display. */
  price: number;
  /** Pay token symbol, e.g. mUSD1. */
  token: string;
  /** Pay token address, from the wire; the market rows carry it even when
   *  the symbol is absent. */
  tokenAddress?: string;
  /** Pay token decimals, straight off the wire when present. */
  tokenDecimals?: number;
  /** Unix seconds when the maturation window elapses; the source of the
   *  "matures in N h" hint for a not-yet-matured row. */
  maturesAt: number;
  /** Server boolean: the only authority on maturity, never the browser. */
  matured: boolean;
  /** Server boolean: true when this is the connected customer's own row. */
  mine: boolean;
  /** The seller's base58 wallet - where the buyer's payment lands. Absent
   *  only on legacy rows; the buy path requires it. */
  seller?: string;
}
