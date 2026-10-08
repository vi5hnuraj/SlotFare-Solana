import {
  EnvironmentProviders,
  InjectionToken,
  inject,
  makeEnvironmentProviders,
  Service,
} from '@angular/core';
import {
  HttpClient,
  HttpErrorResponse,
  HttpHeaders,
  provideHttpClient,
  withFetch,
} from '@angular/common/http';
import { firstValueFrom } from 'rxjs';
import { WalletService } from './wallet';
import { apiBaseUrl } from './runtime-config';
import { environment } from '../../environments/environment';
import { KEYS, OPENING_BALANCE, OPENING_LOCKED, USAGE, WALLET_ADDRESS } from './mock-data';
import { merkleRoot, siblingPath, usageLeafHash, type CanonicalLeaf, type ProofStep } from './commitment';
import { Listing, MarketListing, Model, ModelCapability, ModelKind, ModelPricing, ServingPath } from './models';

/**
 * The console API, typed once. `HttpSlotfareApi` talks to the metering
 * backend; `MockSlotfareApi` serves the demo fixtures. Both satisfy this
 * interface and both are exercised by the same conformance suite in
 * `api.spec.ts`, so a drift between them is a test failure, not a
 * surprise at runtime.
 *
 * Money is integer credits in transit, always: no floats, no strings,
 * no cents. Formatting to dollars happens in `pricing.ts` and nowhere
 * else.
 */

// ---- transport types ---------------------------------------------------

/** Response of `POST /console/session/nonce`. */
export interface Nonce {
  nonce: string;
  statement: string;
  issuedAt: string;
  expiresAt: string;
}

/** Response of `POST /console/session/verify` on success. */
export interface VerifyResult {
  accountId: string;
  token: string;
  expiresAt: string;
}

/** Response of `GET /console/account`. */
export interface Account {
  accountId: string;
  address: string;
  balanceCredits: number;
  /** Whole credits locked by open sell listings; never spendable. */
  lockedCredits: number;
  totalSpentCredits: number;
  createdAt: string;
  /** The inference base URL the backend advertises; '' when it does not. */
  baseUrl: string;
}

/** One API key as stored server-side. */
export interface Key {
  id: string;
  label: string;
  createdAt: string;
  lastUsedAt: string | null;
  requestCount: number;
  revoked: boolean;
}

/** Response of `POST /console/keys`. The plaintext is shown once and never stored. */
export interface CreatedKey {
  key: Key;
  plaintext: string;
}

/** The charge facts that make up a commitment leaf: enough to recompute
 *  the leaf hash, and nothing the server would have to re-send. */
export interface UsageCharge {
  occurredAt: string;
  keyId: string;
  model: string;
  promptTokens: number | null;
  cachedTokens: number | null;
  completionTokens: number | null;
  /** Integer credits in wire units (micro-credits per whole credit). */
  credits: number;
}

/** One usage row as returned by the metering API. */
export interface UsageRowT extends UsageCharge {
  id: string;
  status: string;
  latencyMs: number | null;
  error: string | null;
  /** Settlement facts; present only on a settled row, all three together.
   *  A metered row without them is still awaiting settlement. */
  batchIndex: number | null;
  root: string | null;
  commitTx: string | null;
}

/**
 * The wire's distinct lists are values only: an empty or whitespace-only
 * entry is the server's placeholder for the "All" option, which the
 * consuming dropdown owns (it prepends its own single '' entry), and a
 * duplicate would render it twice - so both are dropped here, once, at
 * the boundary every consumer sees. First-seen order is kept.
 */
function normalizeFacetList(v: unknown): string[] {
  if (!Array.isArray(v)) return [];
  const seen = new Set<string>();
  const out: string[] = [];
  for (const x of v) {
    if (typeof x !== 'string') continue;
    const t = x.trim();
    if (t === '' || seen.has(t)) continue;
    seen.add(t);
    out.push(t);
  }
  return out;
}

/** The account's distinct filter values the paged read reports: the key
 *  prefixes and the model ids, one option per dropdown. */
export interface UsageFacets {
  prefixes: string[];
  models: string[];
}

/** A page of usage rows. `nextCursor` is the before-id cursor of the
 *  unnumbered read (the id of the last returned row, null at the oldest).
 *  `total` and `facets` ride the numbered read (?page): the count of rows
 *  matching the filters, and the account's distinct key prefixes / models
 *  that populate the log's dropdowns. */
export interface UsagePage {
  rows: UsageRowT[];
  nextCursor: number | null;
  total?: number;
  facets?: UsageFacets;
}

/** A page of listing rows; `nextCursor` is the id of the last returned row,
 *  null when the server reports no more history. It is a decimal string,
 *  not a number: a listing id is the PDA's last 8 bytes, half of which are
 *  above 2^53 and would round to a wrong row. */
export interface ListingPage {
  rows: Listing[];
  nextCursor: string | null;
}

/** The model catalog read: the rows plus the base URL the endpoint
 *  advertises on the same payload; '' when the wire carries none. */
export interface ModelList {
  models: Model[];
  /** The inference base URL the endpoint advertises; '' when it does not. */
  baseUrl: string;
}

/**
 * Server-side query for `GET /console/usage`. Without `page` the read is the
 * unnumbered batch form: `before` walks back by row id, `limit` caps it, and
 * `key` / `search` / `days` are the demo stand-ins. With `page` it switches
 * to the numbered form (T01): `page` + `limit` select one page of the
 * filtered set, and `key` (prefix), `model`, `from` / `to` (unix seconds,
 * inclusive) and `errors` narrow it. The two are exclusive on the wire; both
 * present is a bad request the server rejects.
 */
export interface UsageQuery {
  limit?: number;
  /** The key prefix (or the legacy 'errors' / 'all' sentinels). */
  key?: string;
  /** A model id; narrows to that model's rows. */
  model?: string;
  /** Start of the range in unix seconds, inclusive. */
  from?: number;
  /** End of the range in unix seconds, inclusive. */
  to?: number;
  /** Narrow to failed calls only (the Error chip). */
  errors?: boolean;
  search?: string;
  days?: number;
  /** The 1-based page number of the numbered read. */
  page?: number;
  /** A before-id cursor: rows strictly older than this row id, so the next
   *  read pages back through the history. Exclusive with `page`. */
  before?: number;
}

/** The inclusion proof of one settled usage row against its batch root:
 *  the charge facts enough to recompute the leaf, the leaf's index in the
 *  batch, the sibling hashes from leaf to root, and the root itself. */
export interface UsageProof extends UsageCharge {
  index: number;
  path: string[];
  root: string;
  /** The server's own scheme when the proof came off the wire: slotfared's ten
   *  canonical words, its leaf hash and the directed sibling steps. The mock
   *  serves the index/path form above instead. */
  canonical?: { leaf: CanonicalLeaf; leafHash: string; steps: ProofStep[] };
}

/** GET /usage/{id}/proof as slotfared sends it. */
export interface UsageProofWire {
  settled: boolean;
  leaf: CanonicalLeaf;
  leafHash: string;
  nonce?: string;
  proof?: { hash: string; direction: string }[];
  batch?: { root: string; firstUsageId: number; lastUsageId: number; credits: number; txHash: string; committedAt: number; batchIndex: number };
}

/** Map the wire proof onto the client's shape; an unsettled row has none. */
export function fromUsageProofWire(w: UsageProofWire): UsageProof {
  if (!w.settled || !w.batch) {
    throw new ApiError(409, 'unsettled', 'Awaiting settlement: no commitment yet');
  }
  const steps: ProofStep[] = (w.proof ?? []).map((s) => ({
    hash: s.hash,
    direction: s.direction === 'left' ? 'left' : 'right',
  }));
  return {
    occurredAt: new Date(w.leaf.ts * 1000).toISOString(),
    keyId: '',
    model: w.leaf.model,
    promptTokens: w.leaf.promptTokens,
    cachedTokens: w.leaf.cachedTokens,
    completionTokens: w.leaf.completionTokens,
    credits: w.leaf.creditsMicro,
    index: w.leaf.id - w.batch.firstUsageId,
    path: [],
    root: w.batch.root,
    canonical: { leaf: w.leaf, leafHash: w.leafHash, steps },
  };
}

/** Response of `GET /console/funding/:purchaseId` or one entry of
 *  `GET /console/funding`. */
export interface FundingStatus {
  purchaseId: string;
  state: 'pending' | 'confirmed' | 'settled' | 'failed';
  ethTx: string;
  ethBlock: number | null;
  attestedHeight: number | null;
  chainTip: number | null;
  builderCeiling: number | null;
  ccTx: string | null;
  ccBlock: number | null;
  credits: number | null;
  amountUsd: number;
  token: string;
  startedAt: string;
  updatedAt: string;
  /** The source chain this purchase was paid on; 0 is the Solana row. */
  chainKey: number;
  /** The source chain's display name, as reported by the backend. */
  chainName: string;
}

/** One payment source chain as the metering backend reports it. */
export interface ChainInfo {
  /** 0 is the Solana row - the only row a Slotfare deployment serves. */
  chainKey: number;
  name: string;
  /** The EVM chain id, as a plain number. */
  chainId: number;
  /** The treasury's USDC token account - where a deposit goes. */
  deposit: string;
  /** The block-explorer base URL for that chain (no trailing slash). */
  explorer: string;
  /** The public source-chain RPC endpoint the console's reads ride on
   *  (EP-032-T01); absent when the wire does not serve one - the reads
   *  then fall back to the wallet provider. */
  rpc?: string;
}

/** One source chain's attestation health as the metering backend reports
 *  it: where the provers' attested tip sits against the chain's head. */
export interface AttestChain {
  /** The source chain key; 0 (the Solana row) is never a row. */
  chainKey: number;
  name: string;
  /** False when any read for this chain failed: the row is "unreachable".
   *  Such a row carries its key and name only, every other field zero. */
  known: boolean;
  /** The attested tip height; 0 when unknown or nothing is attested yet. */
  attestedTip: number;
  /** The chain's current head; 0 when unknown. */
  head: number;
  /** The lag in blocks; 0 when undetermined. */
  lagBlocks: number;
  /** The lag in seconds; 0 when undetermined (set only when the server
   *  could read both block timestamps). */
  lagSeconds: number;
}

/** Wire shape of one entry of `GET /console/attestation`. */
export interface AttestationChainWire {
  chainKey?: unknown;
  name?: unknown;
  known?: unknown;
  attestedTip?: unknown;
  head?: unknown;
  lagBlocks?: unknown;
  lagSeconds?: unknown;
}

/** Map one wire attestation row onto the console's `AttestChain`. Defensive
 *  defaults keep a partial row from ever rendering `undefined`: numbers are
 *  finite and non-negative else 0, `known` is a boolean else false. */
export function fromAttestationWire(c: AttestationChainWire): AttestChain {
  const n = (v: unknown): number =>
    typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : 0;
  return {
    chainKey: n(c.chainKey),
    name: typeof c.name === 'string' ? c.name : '',
    known: c.known === true,
    attestedTip: n(c.attestedTip),
    head: n(c.head),
    lagBlocks: n(c.lagBlocks),
    lagSeconds: n(c.lagSeconds),
  };
}

/** Wire shape of one entry of `GET /console/chains`. */
export interface ChainInfoWire {
  chainKey?: unknown;
  name?: unknown;
  chainId?: unknown;
  deposit?: unknown;
  explorer?: unknown;
  /** The optional public source-chain RPC endpoint (EP-032-T01); mapped
   *  defensively in `fromChainWire`. */
  rpc?: unknown;
}

/** Map one wire chain onto the console's `ChainInfo`. Defensive defaults keep
 *  a partial row from ever rendering `undefined`; a malformed `swap` object
 *  is dropped rather than half-rendered. */
export function fromChainWire(c: ChainInfoWire): ChainInfo {
  const chain: ChainInfo = {
    chainKey: typeof c.chainKey === 'number' ? c.chainKey : 0,
    name: typeof c.name === 'string' ? c.name : '',
    chainId: typeof c.chainId === 'number' ? c.chainId : 0,
    deposit: typeof c.deposit === 'string' ? c.deposit : '',
    explorer: typeof c.explorer === 'string' ? c.explorer : '',
  };
  const rpc = typeof c.rpc === 'string' && c.rpc !== '' ? c.rpc : undefined;
  if (rpc !== undefined) chain.rpc = rpc;
  return chain;
}

/** Wire shape of `GET /console/funding`: the two frontiers plus every
 *  uncredited purchase with the stage it has reached (1 recorded, 2 confirmed,
 *  3 attested, 4 provable, 5 credited). */
export interface FundingView {
  tip: number;
  ceiling: number;
  purchases: PurchaseWire[];
}

/** One purchase as the metering backend reports it. */
export interface PurchaseWire {
  sourceTx: string;
  sourceBlock: number;
  attestedHeight: number;
  ceiling: number;
  stage: number;
  depositId: string;
  token: string;
  amountUsd: number;
  startedAt: string;
  credited: boolean;
  ccTx: string;
  ccBlock: number;
  creditedAt: string;
  credits: number;
  chainKey?: number;
  chainName?: string;
}

/**
 * Map one wire purchase onto the trace's `FundingStatus`. Every field is a
 * fact the server reports; a mint field is null until the purchase is
 * credited.
 */
export function fromPurchaseView(p: PurchaseWire, v: FundingView): FundingStatus {
  const settled = p.credited || p.stage >= 5;
  const confirmed = p.stage >= 2 && p.sourceBlock > 0;
  const attested = p.stage >= 3;
  const out: FundingStatus = {
    // A direct purchase's paying tx and mint tx are the same signature - that
    // signature is the id the console announced it under, so the local
    // placeholder can reconcile.
    purchaseId: p.sourceTx || p.ccTx || '',
    state: settled ? 'settled' : confirmed ? 'confirmed' : 'pending',
    ethTx: p.sourceTx,
    ethBlock: confirmed || settled ? p.sourceBlock : null,
    attestedHeight: attested ? (p.attestedHeight > 0 ? p.attestedHeight : p.sourceBlock) : null,
    chainTip: v.tip > 0 ? v.tip : null,
    builderCeiling: attested && p.ceiling > 0 ? p.ceiling : null,
    ccTx: settled && p.ccTx ? p.ccTx : null,
    ccBlock: settled && p.ccBlock > 0 ? p.ccBlock : null,
    credits: settled ? p.credits : null,
    amountUsd: p.amountUsd ?? 0,
    token: p.token ?? '',
    startedAt: p.startedAt ?? '',
    // For a settled purchase this is when the credits landed, so the
    // trace's elapsed time is start-to-credit, not start-to-now.
    updatedAt: (settled && p.creditedAt) || p.startedAt || '',
    chainKey: p.chainKey ?? 0,
    chainName: p.chainName ?? '',
  };
  return out;
}

/** Wire shape of `GET /console/account`: the ledger position in integer
 *  micro-credits (10,000 per credit), so sub-credit balances survive the
 *  wire. */
export interface AccountWire {
  accountId: string;
  mintedMicro: number;
  settledMicro: number;
  pendingMicro: number;
  lockedMicro: number;
  spendableMicro: number;
  /** The advertised inference base URL; optional until the server carries it. */
  baseUrl?: unknown;
}

/** Wire shape of one entry of `GET /console/listings`: the account's
 *  market rows in every state, newest first. */
export interface ListingWire {
  /** Decimal string: the PDA's last 8 bytes, a full uint64 on the wire. */
  listingId: string;
  credits: number;
  /** The seller's base58 wallet (empty on legacy rows). */
  seller?: string;
  payTokenSymbol: string;
  /** Pay token address; the server carries it when it can resolve one. */
  payToken?: string;
  /** Pay token decimals; the server carries them when it knows them. */
  payTokenDecimals?: number;
  price: number;
  matured: boolean;
  state: 'open' | 'cancelled' | 'sold';
  role: 'selling' | 'buying';
  /** The listing transaction hash; the D57 local-pending row is matched
   *  against it, never against the numeric id. */
  listedTx: string;
  listedAt: string;
  /** The block the listing transaction landed in. */
  listedBlock?: number;
  /** Unix seconds of the maturation deadline that gated the sale. */
  maturesAt?: number;
  /** The sale/cancel move; Go `omitempty` drops zero blocks, so an absent
   *  or 0 value means "not there". */
  soldTx?: string;
  soldBlock?: number;
  soldAt?: string;
  cancelledTx?: string;
  cancelledBlock?: number;
  cancelledAt?: string;
}

/** Map one wire listing onto the console's `Listing` row. The id is the
 *  stringified on-chain listing id, what `cancel(listingId)` takes; the
 *  local-pending twin is recognised by `listedTx`, not by that id. */
export function fromListingWire(w: ListingWire): Listing {
  return {
    id: String(w.listingId),
    listedTx: w.listedTx ?? '',
    state: w.state,
    role: w.role,
    credits: w.credits,
    price: w.price,
    token: w.payTokenSymbol,
    tokenAddress: typeof w.payToken === 'string' ? w.payToken : '',
    tokenDecimals:
      typeof w.payTokenDecimals === 'number' ? w.payTokenDecimals : undefined,
    matured: w.matured,
    createdAt: w.listedAt ?? '',
    listedBlock:
      typeof w.listedBlock === 'number' && Number.isFinite(w.listedBlock) && w.listedBlock > 0
        ? w.listedBlock
        : undefined,
    maturesAt:
      typeof w.maturesAt === 'number' && Number.isFinite(w.maturesAt) && w.maturesAt > 0
        ? w.maturesAt
        : undefined,
    soldTx: typeof w.soldTx === 'string' ? w.soldTx : undefined,
    soldBlock:
      typeof w.soldBlock === 'number' && Number.isFinite(w.soldBlock) && w.soldBlock > 0
        ? w.soldBlock
        : undefined,
    soldAt: typeof w.soldAt === 'string' ? w.soldAt : undefined,
    cancelledTx: typeof w.cancelledTx === 'string' ? w.cancelledTx : undefined,
    cancelledBlock:
      typeof w.cancelledBlock === 'number' && Number.isFinite(w.cancelledBlock) && w.cancelledBlock > 0
        ? w.cancelledBlock
        : undefined,
    cancelledAt: typeof w.cancelledAt === 'string' ? w.cancelledAt : undefined,
  };
}

/** Wire shape of one entry of `GET /console/market`: an open listing on the
 *  market from any seller, read-only. `mine` is the server's ownership
 *  signal (accounts are hashed wallets; the server is the one that knows);
 *  `seller` is the base58 wallet a buyer pays. */
export interface MarketListingWire {
  /** Decimal string: the PDA's last 8 bytes, a full uint64 on the wire. */
  listingId: string;
  credits: number;
  payToken: string;
  payTokenSymbol: string;
  /** Pay token decimals; the server carries them when it knows them. */
  payTokenDecimals?: number;
  price: number;
  /** Unix seconds when the maturation window elapses. */
  maturesAt: number;
  matured: boolean;
  mine: boolean;
  /** The seller's base58 wallet - where a buyer's payment lands. Public
   *  on-chain data, and what the buyer's transfer needs before the
   *  signature exists. */
  seller?: string;
}

/** Map one wire market listing onto the buyer's `MarketListing` row. The id
 *  is the stringified on-chain listing id, what `buy(listingId)` takes.
 *  And `mine` passes straight through from the wire; there is no
 *  client-side address compare. */
export function fromMarketListingWire(w: MarketListingWire): MarketListing {
  return {
    id: String(w.listingId),
    credits: w.credits,
    price: w.price,
    token: w.payTokenSymbol,
    tokenAddress: typeof w.payToken === 'string' ? w.payToken : '',
    tokenDecimals:
      typeof w.payTokenDecimals === 'number' ? w.payTokenDecimals : undefined,
    maturesAt: w.maturesAt,
    matured: w.matured,
    mine: w.mine,
    seller: typeof w.seller === 'string' && w.seller !== '' ? w.seller : undefined,
  };
}

/** Wire shape of one entry of `GET /console/models`: a model's credit
 *  pricing, whole credits per million tokens. */
export interface ModelPricingWire {
  uncachedInput?: unknown;
  cachedInput?: unknown;
  output?: unknown;
}

/** Wire shape of one entry of `GET /console/models`. The optional metadata
 *  fields are the console's contract with the server (EP-022-T01); every one
 *  is `unknown` and mapped defensively so a partial or malformed row stays
 *  clean. */
export interface ModelWire {
  id?: unknown;
  name?: unknown;
  pricing?: ModelPricingWire;
  vendor?: unknown;
  contextLength?: unknown;
  type?: unknown;
  servingPath?: unknown;
  description?: unknown;
  parameters?: unknown;
  capabilities?: unknown;
  moe?: unknown;
}

/** Map one wire model onto the console's `Model`. A missing or non-numeric
 *  rate becomes 0 so the arithmetic never produces NaN; an empty id is left
 *  for the list reader to drop. The optional metadata fields are added to
 *  the result only when the wire carries a well-formed value, so a bare
 *  row passes through without invented keys. */
export function fromModelWire(m: ModelWire): Model {
  const p = m.pricing ?? {};
  const num = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
  const pricing: ModelPricing = {
    uncachedInput: num(p.uncachedInput),
    cachedInput: num(p.cachedInput),
    output: num(p.output),
  };
  const out: Model = {
    id: typeof m.id === 'string' ? m.id : '',
    name: typeof m.name === 'string' ? m.name : '',
    pricing,
  };
  const str = (v: unknown): string | undefined =>
    typeof v === 'string' && v !== '' ? v : undefined;
  const vendor = str(m.vendor);
  if (vendor !== undefined) out.vendor = vendor;
  const description = str(m.description);
  if (description !== undefined) out.description = description;
  const parameters = str(m.parameters);
  if (parameters !== undefined) out.parameters = parameters;
  if (
    typeof m.contextLength === 'number' &&
    Number.isInteger(m.contextLength) &&
    m.contextLength > 0
  ) {
    out.contextLength = m.contextLength;
  }
  const kinds: readonly string[] = ['chat', 'embeddings', 'reranker'];
  if (typeof m.type === 'string' && kinds.includes(m.type)) {
    out.kind = m.type as ModelKind;
  }
  const paths: readonly string[] = ['standard', 'priority', 'fast'];
  if (typeof m.servingPath === 'string' && paths.includes(m.servingPath)) {
    out.servingPath = m.servingPath as ServingPath;
  }
  if (Array.isArray(m.capabilities)) {
    const caps: readonly string[] = [
      'text',
      'image',
      'video',
      'audio',
      'vision',
      'function-calling',
    ];
    const list = m.capabilities.filter(
      (c): c is ModelCapability => typeof c === 'string' && caps.includes(c),
    );
    if (list.length > 0) out.capabilities = list;
  }
  // A boolean only: absent or non-boolean omits the key, never invents one.
  if (typeof m.moe === 'boolean') out.moe = m.moe;
  return out;
}

/** Wire shape of one entry of `GET /console/keys`. */
export interface KeyWire {
  prefix: string;
  label: string;
  createdAt: string;
  lastUsed: string | null;
  requestCount: number;
  revoked: boolean;
}

/** Wire shape of `POST /console/keys`: the plaintext, shown once. */
export interface CreatedKeyWire {
  key: string;
  prefix: string;
  label: string;
  createdAt: string;
}

/** Wire shape of one `GET /console/usage` entry and of a `usage` stream event.
 *  The credit field is integer micro-credits, so a sub-credit charge is
 *  never rounded to zero on the wire. */
export interface UsageWire {
  timestamp: string;
  service: string;
  model: string;
  promptTokens: number;
  cachedTokens: number;
  completionTokens: number;
  creditsMicro: number;
  status: string;
  keyPrefix: string;
  /** The stored row id, the proof route's handle; absent on older servers. */
  id?: unknown;
  /** Null for rows written before the column existed; the key stays present.
   *  Snake case on the wire (Go json tag), unlike the camelCase siblings. */
  duration_ms?: number | null;
  /** Settlement facts; on settled rows only, and only when all three are
   *  present. Absent (or non-conforming) means awaiting settlement. */
  batchIndex?: unknown;
  root?: unknown;
  commitTx?: unknown;
}

/** Usage rows travel through the store as integer micro-credits: the wire
 *  already carries them, so the adapter passes them through and the store
 *  divides back down to credits for display. */
export const MICRO_CREDITS_PER_CREDIT = 10_000;

/** The address is not on the wire: it is the wallet the session was signed
 *  with, so the caller supplies it. */
export function fromAccountWire(a: AccountWire, address: string): Account {
  return {
    accountId: a.accountId,
    address,
    balanceCredits: a.spendableMicro / MICRO_CREDITS_PER_CREDIT,
    lockedCredits: (a.lockedMicro ?? 0) / MICRO_CREDITS_PER_CREDIT,
    totalSpentCredits:
      ((a.settledMicro ?? 0) + (a.pendingMicro ?? 0)) / MICRO_CREDITS_PER_CREDIT,
    createdAt: '',
    baseUrl:
      typeof a.baseUrl === 'string' && a.baseUrl !== '' ? a.baseUrl : '',
  };
}

export function fromKeyWire(k: KeyWire): Key {
  return {
    id: k.prefix,
    label: k.label,
    createdAt: k.createdAt,
    lastUsedAt: k.lastUsed,
    requestCount: k.requestCount,
    revoked: k.revoked,
  };
}

export function fromCreatedKeyWire(c: CreatedKeyWire): CreatedKey {
  return {
    key: { id: c.prefix, label: c.label, createdAt: c.createdAt, lastUsedAt: null, requestCount: 0, revoked: false },
    plaintext: c.key,
  };
}

export function fromUsageWire(u: UsageWire): UsageRowT {
  // The stored row id is the handle for GET /usage/{id}/proof. Older servers
  // sent none, so the client falls back to a stable synthetic id built from
  // the fields that pin a row down across pages and refreshes.
  return {
    id:
      typeof u.id === 'number' && Number.isInteger(u.id)
        ? String(u.id)
        : `${u.timestamp}:${u.keyPrefix}:${u.model}:${u.creditsMicro}`,
    occurredAt: u.timestamp,
    keyId: u.keyPrefix,
    model: u.model,
    promptTokens: u.promptTokens,
    cachedTokens: u.cachedTokens,
    completionTokens: u.completionTokens,
    credits: u.creditsMicro,
    status: u.status,
    // The wire carries no error message; `status` is the only error-ness
    // source, so it is mapped once here and nowhere else.
    error: u.status === 'error' ? 'error' : null,
    // An absent key and a JSON null both read as null; a missing duration is
    // never fabricated as zero.
    latencyMs: typeof u.duration_ms === 'number' ? u.duration_ms : null,
    // Settlement facts map defensively like the duration: absent or
    // non-conforming values read as null, so a partial row can never render
    // half a commitment.
    batchIndex:
      typeof u.batchIndex === 'number' && Number.isInteger(u.batchIndex)
        ? u.batchIndex
        : null,
    root: typeof u.root === 'string' && u.root !== '' ? u.root : null,
    commitTx: typeof u.commitTx === 'string' && u.commitTx !== '' ? u.commitTx : null,
  };
}

/**
 * The unix-seconds of a usage row's timestamp, for the mock's from/to
 * stand-in. An unparseable value (the demo fixtures carry bare
 * `HH:MM:SS` times) reads as 0, so a real date bound excludes it rather
 * than fabricating a range.
 */
function usageUnixSec(ts: string): number {
  const t = Date.parse(ts);
  return Number.isNaN(t) ? 0 : Math.floor(t / 1000);
}

// ---- errors ------------------------------------------------------------

/**
 * Every failure of the API surface, typed. `status` is the HTTP status,
 * or 0 for non-HTTP failures (network down, offline). `code` is the
 * machine-readable reason from the body when present.
 */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

// ---- the contract ------------------------------------------------------

export interface SlotfareApi {
  nonce(address: string): Promise<Nonce>;
  verify(address: string, nonce: string, signature: string): Promise<VerifyResult>;
  account(): Promise<Account>;
  keys(): Promise<Key[]>;
  createKey(label: string): Promise<CreatedKey>;
  revokeKey(id: string): Promise<void>;
  usage(query?: UsageQuery): Promise<UsagePage>;
  /** The inclusion proof of one settled usage row against its batch root.
   *  Rejected with a 404 ApiError for unknown ids and pending rows. */
  usageProof(rowId: string): Promise<UsageProof>;
  funding(purchaseId: string): Promise<FundingStatus>;
  fundingList(): Promise<FundingStatus[]>;
  /** The payment source chains this account can pay on. */
  chains(): Promise<ChainInfo[]>;
  /** Per source chain attestation health; empty array on this single-chain
   *  deployment; 501 when the server has no feeder. */
  attestation(): Promise<AttestChain[]>;
  /** The models this endpoint serves, each with its per-million credit
   *  pricing, plus the base URL the endpoint advertises on the payload. */
  models(): Promise<ModelList>;
  /** The account's market listings in every state, newest first; `before`
   *  pages to older rows (decimal string, per ListingPage). */
  listings(before?: string): Promise<ListingPage>;
  /** Open listings on the market from any seller, newest first. */
  market(): Promise<MarketListing[]>;
  /** Where a USDC deposit lands: treasury token account, mint, cluster and
   *  the conversion figures every payment preview shows. 501 when Solana is
   *  not configured. */
  depositTarget(): Promise<DepositTarget>;
  /** Open a listing: whole credits locked at `price` in USDC base units
   *  (6 decimals). Returns the created row. */
  createListing(body: { credits: number; price: number }): Promise<Listing>;
  /** Build one market write as an unsigned transaction for this wallet, with
   *  the caller as fee payer. The daemon never holds a user's key, so every
   *  market instruction goes through here before it can be signed. 501 when
   *  the deployment has no escrow program. */
  prepareMarket(body: MarketPrepareBody): Promise<PreparedMarketTx>;
  /** Record a market write the program has already performed, against the
   *  signed transaction. The server re-reads the listing account before it
   *  writes a row, so a confirm that never landed on chain is refused.
   *  Returns the row after the move. */
  confirmMarket(body: MarketConfirmBody): Promise<Listing>;
  /** Withdraw one's own open listing; returns the row after the move. */
  cancelListing(id: string): Promise<Listing>;
  /** Settle a sale against an already-sent USDC transfer: the server
   *  verifies `signature` nets the price buyer-to-seller, then moves the
   *  credits. Returns the sold row. */
  payListing(id: string, signature: string): Promise<Listing>;
}

/** The three market writes the console accepts, in the order the escrow
 *  program expects them. */
export type MarketAction = 'list' | 'buy' | 'cancel';

/** Body of `POST /console/listings/onchain/prepare`: a list carries the
 *  terms, a buy and a cancel name the row they act on. */
export interface MarketPrepareBody {
  action: MarketAction;
  /** Required for buy and cancel, as a decimal string: a listing id is a
   *  full uint64 and a JS number rounds any integer above 2^53. */
  listingId?: string;
  /** Required for list: whole credits to escrow. */
  credits?: number;
  /** Required for list: the price in pay-token base units. */
  price?: number;
}

/** One prepared market transaction: the nonce the instruction carries and
 *  the base64 wire transaction for the wallet to sign. The nonce comes back
 *  on confirm so the two halves cannot be crossed. It is a decimal string,
 *  not a number: a nonce is a full 64-bit draw and a JS number rounds any
 *  integer above 2^53, which would echo back a different PDA seed. */
export interface PreparedMarketTx {
  action: MarketAction;
  nonce: string;
  transaction: string;
}

/** Body of `POST /console/listings/onchain/confirm`: the write, the nonce
 *  prepare handed out (decimal string, per PreparedMarketTx), and the
 *  signature the wallet produced. */
export interface MarketConfirmBody {
  action: MarketAction;
  /** Required for buy and cancel, as a decimal string (per
   *  MarketPrepareBody). */
  listingId?: string;
  nonce: string;
  signature: string;
}

/** `GET /console/deposit-target`: where a deposit lands and at what rate. */
export interface DepositTarget {
  /** The treasury's USDC token account (base58) - the transfer's owner. */
  treasury: string;
  /** The USDC mint (base58) the account holds. */
  mint: string;
  /** Solana cluster the target lives on (devnet, localnet, mainnet-beta). */
  cluster: string;
  /** Public, keyless JSON-RPC endpoint for the cluster. */
  rpc: string;
  /** Explorer transaction-URL base. */
  explorer: string;
  /** The mint's decimals; 6 on every USDC deployment. */
  decimals: number;
  /** Whole credits granted per US dollar. */
  creditsPerUsd: number;
  /** Micro-credits per USDC base unit - the exact scale for previews. */
  microCreditsPerUsdBase: number;
}

/** DI token for the SlotfareApi singleton. */
export const SLOTFARE_API = new InjectionToken<SlotfareApi>('SlotfareApi');

// ---- HTTP implementation -----------------------------------------------

@Service()
export class HttpSlotfareApi implements SlotfareApi {
  private readonly http = inject(HttpClient);
  private readonly walletSvc = inject(WalletService);
  /** Console API base; runtime config wins over the compiled default. */
  private readonly baseUrl = apiBaseUrl();

  /** Bearer headers when a session exists; empty before one does. */
  private headers(): HttpHeaders {
    const t = this.walletSvc.token();
    return t === null
      ? new HttpHeaders()
      : new HttpHeaders({ Authorization: `Bearer ${t}` });
  }

  /** Map any failure to a typed `ApiError`; nothing else escapes. */
  private toApiError(err: unknown): ApiError {
    if (err instanceof HttpErrorResponse) {
      const code =
        typeof err.error?.code === 'string' && err.error.code.length > 0
          ? err.error.code
          : err.statusText || 'error';
      return new ApiError(err.status, code, err.message);
    }
    const msg = err instanceof Error ? err.message : String(err);
    return new ApiError(0, 'network', msg || 'Network request failed');
  }

  private async get<T>(path: string): Promise<T> {
    try {
      return await firstValueFrom(this.http.get<T>(this.baseUrl + '/console' + path, { headers: this.headers() }));
    } catch (e) {
      throw this.toApiError(e);
    }
  }

  private async post<T>(path: string, body: unknown): Promise<T> {
    try {
      return await firstValueFrom(
        this.http.post<T>(this.baseUrl + '/console' + path, body, { headers: this.headers() }),
      );
    } catch (e) {
      throw this.toApiError(e);
    }
  }

  private async delete(path: string): Promise<void> {
    try {
      await firstValueFrom(this.http.delete(this.baseUrl + '/console' + path, { headers: this.headers() }));
    } catch (e) {
      throw this.toApiError(e);
    }
  }

  nonce(address: string): Promise<Nonce> {
    return this.post<Nonce>('/session/nonce', { address });
  }

  verify(address: string, nonce: string, signature: string): Promise<VerifyResult> {
    return this.post<VerifyResult>('/session/verify', { address, nonce, signature });
  }

  account(): Promise<Account> {
    return this.get<AccountWire>('/account').then((a) =>
      fromAccountWire(a, this.walletSvc.address() ?? ''),
    );
  }

  keys(): Promise<Key[]> {
    return this.get<{ keys: KeyWire[] }>('/keys').then((r) => r.keys.map(fromKeyWire));
  }

  createKey(label: string): Promise<CreatedKey> {
    return this.post<CreatedKeyWire>('/keys', { label }).then(fromCreatedKeyWire);
  }

  revokeKey(id: string): Promise<void> {
    return this.delete(`/keys/${id}`);
  }

  usage(query?: UsageQuery): Promise<UsagePage> {
    const parts: string[] = [];
    // The numbered read (?page) and the cursor read (?before) are exclusive
    // on the wire; the store sends one or the other, never both.
    if (query?.page !== undefined) parts.push(`page=${query.page}`);
    if (query?.limit !== undefined) parts.push(`limit=${query.limit}`);
    if (query?.key === 'errors') parts.push('errors=true');
    else if (query?.key && query.key !== 'all') parts.push(`prefix=${encodeURIComponent(query.key)}`);
    if (query?.model) parts.push(`model=${encodeURIComponent(query.model)}`);
    if (query?.from !== undefined) parts.push(`from=${query.from}`);
    if (query?.to !== undefined) parts.push(`to=${query.to}`);
    if (query?.errors) parts.push('errors=true');
    if (query?.before !== undefined) parts.push(`before=${query.before}`);
    const q = parts.length > 0 ? `?${parts.join('&')}` : '';
    return this.get<{ usage: UsageWire[]; nextCursor?: number; total?: number; facets?: { prefixes?: unknown; models?: unknown } }>(`/usage${q}`).then((r) => {
      const page: UsagePage = {
        rows: r.usage.map(fromUsageWire),
        // The server omits the cursor when the history is exhausted.
        nextCursor: typeof r.nextCursor === 'number' ? r.nextCursor : null,
      };
      // total / facets only ride the numbered read; absent on the cursor form
      // so a paged consumer never reads them off an unnumbered response.
      if (typeof r.total === 'number') page.total = r.total;
      if (r.facets !== undefined) {
        // Normalized at the boundary: no placeholder ('') or duplicate
        // entries ever reach a consumer's option list.
        page.facets = {
          prefixes: normalizeFacetList(r.facets.prefixes),
          models: normalizeFacetList(r.facets.models),
        };
      }
      return page;
    });
  }

  async usageProof(rowId: string): Promise<UsageProof> {
    return fromUsageProofWire(await this.get<UsageProofWire>(`/usage/${rowId}/proof`));
  }

  funding(purchaseId: string): Promise<FundingStatus> {
    return this.get<FundingStatus>(`/funding/${purchaseId}`);
  }

  fundingList(): Promise<FundingStatus[]> {
    return this.get<FundingView>('/funding').then((v) =>
      v.purchases.map((p) => fromPurchaseView(p, v)),
    );
  }

  chains(): Promise<ChainInfo[]> {
    return this.get<{ chains: ChainInfoWire[] }>('/chains').then((r) =>
      (r.chains ?? []).map(fromChainWire),
    );
  }

  attestation(): Promise<AttestChain[]> {
    return this.get<{ chains: AttestationChainWire[] }>('/attestation').then(
      (r) => (r.chains ?? []).map(fromAttestationWire),
    );
  }

  models(): Promise<ModelList> {
    return this.get<{ models: ModelWire[]; baseUrl?: unknown }>('/models').then(
      (r) => ({
        models: (r.models ?? [])
          .map(fromModelWire)
          .filter((m) => m.id !== ''),
        baseUrl:
          typeof r.baseUrl === 'string' && r.baseUrl !== '' ? r.baseUrl : '',
      }),
    );
  }

  listings(before?: string): Promise<ListingPage> {
    const q = before !== undefined ? `?before=${before}` : '';
    return this.get<{ listings: ListingWire[]; nextCursor?: string }>(`/listings${q}`).then((r) => ({
      rows: r.listings.map(fromListingWire),
      // The server omits the cursor when the history is exhausted.
      nextCursor: typeof r.nextCursor === 'string' ? r.nextCursor : null,
    }));
  }

  market(): Promise<MarketListing[]> {
    return this.get<{ market: MarketListingWire[] }>('/market').then((r) =>
      (r.market ?? []).map(fromMarketListingWire),
    );
  }

  depositTarget(): Promise<DepositTarget> {
    return this.get<DepositTarget>('/deposit-target');
  }

  createListing(body: { credits: number; price: number }): Promise<Listing> {
    return this.post<ListingWire>('/listings', body).then(fromListingWire);
  }

  prepareMarket(body: MarketPrepareBody): Promise<PreparedMarketTx> {
    return this.post<PreparedMarketTx>('/listings/onchain/prepare', body);
  }

  confirmMarket(body: MarketConfirmBody): Promise<Listing> {
    return this.post<ListingWire>('/listings/onchain/confirm', body).then(fromListingWire);
  }

  cancelListing(id: string): Promise<Listing> {
    return this.post<ListingWire>(`/listings/${id}/cancel`, {}).then(fromListingWire);
  }

  payListing(id: string, signature: string): Promise<Listing> {
    return this.post<ListingWire>(`/listings/${id}/pay`, { signature }).then(fromListingWire);
  }
}

// ---- mock implementation -------------------------------------------------

/** The demo's USDC mint and one plausible wallet: the only addresses the
 *  fixtures carry now that the market pays in USDC on Solana. */
const MOCK_MINT = 'DemoUSDCmint11111111111111111111111111111111';
const MOCK_SELLER = 'DemoSellerWa11et111111111111111111111111111';

/**
 * Serves the demo fixtures in place of the live backend. A plain class,
 * not a service: it is instantiated by `provideSlotfareApi()` only when
 * `environment.useMock` is true, and never injected directly.
 */
export class MockSlotfareApi implements SlotfareApi {
  /** Internal mutable copy so create/revoke behave like a real server. */
  private internalKeys: Key[] = KEYS.map((k) => ({
    id: k.id,
    label: k.label,
    createdAt: k.created,
    lastUsedAt: k.lastUsedAt,
    requestCount: k.requests,
    revoked: k.revoked,
  }));
  private counter = 0;

  private accountId(address: string): string {
    // Deterministic placeholder derived from the address; the real id is
    // a keccak256 hash, but the mock only needs stability, not identity.
    return '0xmock' + address.toLowerCase().replace(/[^a-z0-9]/g, '').slice(-8);
  }

  async nonce(address: string): Promise<Nonce> {
    return {
      nonce: 'mock-nonce',
      statement: 'Sign in to Slotfare',
      issuedAt: '2026-08-22T20:00:00Z',
      expiresAt: '2026-08-22T20:05:00Z',
    };
  }

  async verify(_address: string, _nonce: string, signature: string): Promise<VerifyResult> {
    if (signature === '') {
      throw new ApiError(401, 'invalid_signature', 'Signature verification failed');
    }
    return {
      accountId: this.accountId(_address),
      token: 'mock-token',
      expiresAt: '2027-08-22T20:00:00Z',
    };
  }

  async account(): Promise<Account> {
    // Whole credits, like every other figure this mock returns; the live
    // wire carries micro-credits and the adapter divides them down.
    const totalSpent = USAGE.reduce((acc, r) => acc + r.credits, 0);
    return {
      accountId: this.accountId(WALLET_ADDRESS),
      address: WALLET_ADDRESS,
      balanceCredits: OPENING_BALANCE,
      lockedCredits: OPENING_LOCKED,
      totalSpentCredits: totalSpent,
      createdAt: '2026-06-14',
      // The offline demo advertises the same base URL the live endpoint
      // screen used to hardcode, so the demo stays populated under the
      // mock flag.
      baseUrl: 'https://api.slotfare.io/v1',
    };
  }

  async keys(): Promise<Key[]> {
    return this.internalKeys.map((k) => ({ ...k }));
  }

  async createKey(label: string): Promise<CreatedKey> {
    this.counter += 1;
    const id = 'mock' + this.counter.toString().padStart(4, '0');
    const key: Key = {
      id,
      label,
      createdAt: '2026-08-22',
      lastUsedAt: null,
      requestCount: 0,
      revoked: false,
    };
    this.internalKeys = [key, ...this.internalKeys];
    return { key: { ...key }, plaintext: `slf_live_mock…${id.slice(-4)}` };
  }

  async revokeKey(id: string): Promise<void> {
    const idx = this.internalKeys.findIndex((k) => k.id === id);
    if (idx === -1) {
      throw new ApiError(404, 'not_found', `Key ${id} not found`);
    }
    this.internalKeys[idx] = { ...this.internalKeys[idx], revoked: true };
  }

  /** The fixture's rows in the wire shape this mock serves: the demo
   *  commitment on each settled row passes straight through, and the
   *  proof tree below is built over these same rows. */
  private usageFixtureRows(): UsageRowT[] {
    return USAGE.map((r, i) => ({
      id: `u${i}`,
      occurredAt: r.time,
      keyId: r.key,
      model: r.model,
      promptTokens: r.prompt,
      cachedTokens: r.cached,
      completionTokens: r.completion,
      credits: Math.round(r.credits * 10_000),
      status: r.result,
      latencyMs: r.latencyMs,
      error: r.error,
      batchIndex: r.commitment?.batchIndex ?? null,
      root: r.commitment?.root ?? null,
      commitTx: r.commitment?.commitTx ?? null,
    }));
  }

  async usage(query?: UsageQuery): Promise<UsagePage> {
    let rows: UsageRowT[] = this.usageFixtureRows();
    // The account's distinct filter values are independent of the active
    // filters (the server's UsageFacets takes only the account id), so they
    // come from the full set before any narrowing.
    const facets: UsageFacets = {
      // Same boundary normalization as the live path, so both
      // implementations agree on the "no placeholder, no duplicates" shape.
      prefixes: normalizeFacetList([...new Set(rows.map((r) => r.keyId))]),
      models: normalizeFacetList([...new Set(rows.map((r) => r.model))]),
    };
    if (query?.key && query.key !== 'all') {
      rows = rows.filter((r) =>
        query.key === 'errors'
          ? r.error !== null
          : r.keyId === query.key,
      );
    }
    if (query?.model) {
      rows = rows.filter((r) => r.model === query.model);
    }
    if (query?.from !== undefined) {
      rows = rows.filter((r) => usageUnixSec(r.occurredAt) >= query.from!);
    }
    if (query?.to !== undefined) {
      rows = rows.filter((r) => usageUnixSec(r.occurredAt) <= query.to!);
    }
    if (query?.errors) {
      rows = rows.filter((r) => r.error !== null);
    }
    if (query?.search) {
      const q = query.search.trim().toLowerCase();
      if (q !== '') {
        rows = rows.filter((r) =>
          [
            r.occurredAt,
            r.keyId,
            r.model,
            r.status,
            r.error ?? '',
          ]
            .join(' ')
            .toLowerCase()
            .includes(q),
        );
      }
    }
    // The matching count is what the numbered read reports as `total`: the
    // filtered set before the page slice, not the page itself.
    const total = rows.length;
    // The cursor pages the account's full history (the server applies
    // `before` after its own row selection), so it cuts the filtered set
    // before the day-range and limit stand-ins slice it.
    if (query?.before !== undefined) {
      const base = rows;
      const idx = base.findIndex((r) => Number(r.id.slice(1)) === query.before);
      rows = idx === -1 ? [] : base.slice(idx + 1);
    }
    if (query?.days !== undefined) {
      // The fixtures carry no timestamps to slice by calendar days, so a
      // shorter range keeps the most recent `days` rows as a stand-in.
      rows = rows.slice(-query.days);
    }
    if (query?.page !== undefined) {
      // The numbered form: one page of the filtered set, newest first. No
      // cursor rides it; `total` and `facets` do.
      const cap = query.limit ?? 25;
      rows = rows.slice((query.page - 1) * cap, query.page * cap);
      return { rows, nextCursor: null, total, facets };
    }
    const preCap = rows;
    const cap = query?.limit ?? 200;
    rows = rows.slice(0, cap);
    return {
      rows,
      nextCursor: this.nextCursorOf(rows, preCap, (r) => Number(r.id.slice(1))),
      total,
      facets,
    };
  }

  /** Page a newest-first fixture array by a before-id cursor: the rows
   *  strictly older than the cursor's row, capped at `cap`. `nextCursor` is
   *  the last returned row's key unless that row is already the oldest of
   *  the whole set, in which case the history is exhausted. */
  private pageBy<T>(
    all: T[],
    before: number | undefined,
    key: (t: T) => number,
    cap: number,
  ): { rows: T[]; nextCursor: number | null } {
    let rows = all;
    if (before !== undefined) {
      const idx = rows.findIndex((r) => key(r) === before);
      rows = idx === -1 ? [] : rows.slice(idx + 1);
    }
    rows = rows.slice(0, cap);
    return { rows, nextCursor: this.nextCursorOf(rows, all, key) };
  }

  /** The last returned row's key when it is not already the oldest of the
   *  full set; null when the batch reaches the oldest row or is empty. */
  private nextCursorOf<T>(rows: T[], all: T[], key: (t: T) => number): number | null {
    const last = rows[rows.length - 1];
    const tail = all[all.length - 1];
    if (last === undefined || tail === undefined) return null;
    const k = key(last);
    return Number.isFinite(k) && k !== key(tail) ? k : null;
  }

  private fundingListData: FundingStatus[] = [];

  /** Internal mutable copy so the sell screen's demo shows real rows; tests
   *  can seed it. Empty by default would hide the empty state, so the demo
   *  carries one open and one sold listing. */
  private listingData: Listing[] = [
    {
      id: '1',
      listedTx: 'MockListSig1111111111111111111111111111111111111',
      state: 'open',
      role: 'selling',
      credits: OPENING_LOCKED,
      price: 500_000_000,
      token: 'USDC',
      tokenAddress: MOCK_MINT,
      tokenDecimals: 6,
      matured: false,
      createdAt: '2026-08-22T20:00:00Z',
    },
    {
      id: '0',
      listedTx: 'MockListSig0000000000000000000000000000000000000',
      state: 'sold',
      role: 'selling',
      credits: 10_000,
      price: 10_000_000_000,
      token: 'USDC',
      tokenAddress: MOCK_MINT,
      tokenDecimals: 6,
      matured: true,
      createdAt: '2026-08-15T09:30:00Z',
      listedBlock: 40201,
      maturesAt: 1755100000,
      soldTx: 'MockSaleSig11111111111111111111111111111111111111',
      soldBlock: 40207,
      soldAt: '2026-08-15T11:45:00Z',
    },
    {
      id: '2',
      listedTx: 'MockListSig2222222222222222222222222222222222222',
      state: 'cancelled',
      role: 'selling',
      credits: 3_000,
      price: 3_000_000_000,
      token: 'USDC',
      tokenAddress: MOCK_MINT,
      tokenDecimals: 6,
      matured: true,
      createdAt: '2026-08-18T10:00:00Z',
      listedBlock: 40185,
      maturesAt: 1755400000,
      cancelledTx: 'MockCancelSig11111111111111111111111111111111111',
      cancelledBlock: 40190,
      cancelledAt: '2026-08-20T08:00:00Z',
    },
    {
      id: '8',
      listedTx: '0xmocksaletx',
      state: 'sold',
      role: 'buying',
      credits: 10_000,
      price: 10_000_000_000,
      token: 'USDC',
      tokenAddress: MOCK_MINT,
      tokenDecimals: 6,
      matured: true,
      createdAt: '2026-08-15T11:45:00Z',
      listedBlock: 40201,
      maturesAt: 1755100000,
      soldTx: '0xmocksaletx',
      soldBlock: 40207,
      soldAt: '2026-08-15T11:45:00Z',
    },
  ];

  async listings(before?: string): Promise<ListingPage> {
    const all = this.listingData.map((l) => ({ ...l }));
    const page = this.pageBy(all, before === undefined ? undefined : Number(before), (l) => Number(l.id), 10);
    return {
      rows: page.rows,
      nextCursor: page.nextCursor === null ? null : String(page.nextCursor),
    };
  }

  /** Open market listings for the demo, served in exactly the shape the
   *  live route serves: envelope key `market`, rows carrying `mine`,
   *  `seller` and unix-seconds `maturesAt`. One buyable foreign row, one
   *  immature foreign row, and one of the customer's own (never
   *  buyable). Public so the parity test can feed it through the same
   *  adapter the live path uses. */
  readonly marketEnvelope: { market: MarketListingWire[] } = {
    market: [
      { listingId: '12', credits: 5_000, payToken: MOCK_MINT, payTokenSymbol: 'USDC', payTokenDecimals: 6, price: 500_000_000, maturesAt: 1756000000, matured: true, mine: false, seller: MOCK_SELLER },
      // maturesAt 2026-10-20: still maturing, so Buy stays disabled.
      { listingId: '9', credits: 10_000, payToken: MOCK_MINT, payTokenSymbol: 'USDC', payTokenDecimals: 6, price: 1_000_000_000, maturesAt: 1792512000, matured: false, mine: false, seller: MOCK_SELLER },
      { listingId: '3', credits: 2_000, payToken: MOCK_MINT, payTokenSymbol: 'USDC', payTokenDecimals: 6, price: 200_000_000, maturesAt: 1755900000, matured: true, mine: true, seller: MOCK_SELLER },
    ],
  };

  async market(): Promise<MarketListing[]> {
    return this.marketEnvelope.market.map(fromMarketListingWire);
  }

  /** The demo's deposit target: one Solana devnet USDC account, mirroring
   *  `GET /console/deposit-target` field for field. */
  async depositTarget(): Promise<DepositTarget> {
    return {
      treasury: 'DemoTreasuryTok1111111111111111111111111111111',
      mint: MOCK_MINT,
      cluster: 'devnet',
      rpc: 'https://api.devnet.solana.com',
      explorer: 'https://explorer.solana.com',
      decimals: 6,
      creditsPerUsd: 1000,
      microCreditsPerUsdBase: 10,
    };
  }

  async createListing(body: { credits: number; price: number }): Promise<Listing> {
    if (!Number.isFinite(body.credits) || body.credits <= 0 || !Number.isFinite(body.price) || body.price <= 0) {
      throw new ApiError(400, 'bad_request', 'credits out of range');
    }
    const id = String(100 + ++this.counter);
    const now = new Date();
    const row: Listing = {
      id,
      listedTx: `MockNewListSig${id}`,
      state: 'open',
      role: 'selling',
      credits: body.credits,
      price: body.price,
      token: 'USDC',
      tokenAddress: MOCK_MINT,
      tokenDecimals: 6,
      matured: false,
      createdAt: now.toISOString(),
      maturesAt: Math.floor(now.getTime() / 1000) + 120,
    };
    this.listingData.unshift(row);
    this.marketEnvelope.market.unshift({
      listingId: id,
      credits: body.credits,
      payToken: MOCK_MINT,
      payTokenSymbol: 'USDC',
      payTokenDecimals: 6,
      price: body.price,
      maturesAt: row.maturesAt ?? 0,
      matured: false,
      mine: true,
      seller: MOCK_SELLER,
    });
    return { ...row };
  }

  /** The offline demo's prepare/confirm pair. There is no cluster to build a
   *  transaction against, so a prepared row carries only a nonce; the market
   *  flows take the mirror path in mock builds and never sign. These exist so
   *  the mock satisfies the same interface a live console does. */
  private readonly preparedMarket = new Map<string, MarketPrepareBody>();

  async prepareMarket(body: MarketPrepareBody): Promise<PreparedMarketTx> {
    if (body.action === 'list') {
      if (!Number.isFinite(body.credits) || (body.credits ?? 0) <= 0) {
        throw new ApiError(400, 'bad_request', 'credits out of range');
      }
      if (!Number.isFinite(body.price) || (body.price ?? 0) <= 0) {
        throw new ApiError(400, 'bad_request', 'price out of range');
      }
    } else if (typeof body.listingId !== 'string' || body.listingId === '') {
      throw new ApiError(400, 'bad_request', 'listingId required');
    }
    const nonce = String(++this.counter);
    this.preparedMarket.set(nonce, body);
    return { action: body.action, nonce, transaction: `MockPreparedTx${nonce}` };
  }

  async confirmMarket(body: MarketConfirmBody): Promise<Listing> {
    if (body.signature.trim() === '') {
      throw new ApiError(400, 'bad_request', 'signature required');
    }
    const prepared = this.preparedMarket.get(body.nonce);
    if (prepared === undefined) {
      throw new ApiError(400, 'bad_request', 'no listing was opened on chain');
    }
    this.preparedMarket.delete(body.nonce);
    const id = prepared.listingId ?? '';
    switch (body.action) {
      case 'list':
        return this.createListing({ credits: prepared.credits ?? 0, price: prepared.price ?? 0 });
      case 'buy':
        return this.payListing(id, body.signature);
      default:
        return this.cancelListing(id);
    }
  }

  async cancelListing(id: string): Promise<Listing> {
    const row = this.listingData.find((l) => l.id === id);
    if (row === undefined) throw new ApiError(404, 'not_found', 'listing not found');
    if (row.state !== 'open') throw new ApiError(409, 'conflict', 'listing is not open');
    row.state = 'cancelled';
    row.cancelledTx = `MockCancelSig${id}`;
    row.cancelledAt = new Date().toISOString();
    this.marketEnvelope.market = this.marketEnvelope.market.filter((m) => String(m.listingId) !== id);
    return { ...row };
  }

  async payListing(id: string, signature: string): Promise<Listing> {
    if (signature === '') throw new ApiError(400, 'bad_request', 'signature required');
    const idx = this.marketEnvelope.market.findIndex((m) => String(m.listingId) === id);
    if (idx === -1) throw new ApiError(409, 'conflict', 'listing no longer open');
    this.marketEnvelope.market.splice(idx, 1);
    const row = this.listingData.find((l) => l.id === id);
    if (row === undefined) throw new ApiError(404, 'not_found', 'listing not found');
    row.state = 'sold';
    row.matured = true;
    row.soldTx = signature;
    row.soldAt = new Date().toISOString();
    return { ...row };
  }

  /** The commitment tree the mock's usageProof serves: the settled fixture
   *  rows (the ones carrying a demo commitment) in fixture order, hashed
   *  with the same scheme the client verifies. Built lazily and cached, so
   *  the served proof and the rows' published commitment root always agree. */
  private usageTree: { settled: UsageRowT[]; leaves: string[]; root: string } | null =
    null;

  private usageTreeOf(): { settled: UsageRowT[]; leaves: string[]; root: string } {
    if (this.usageTree === null) {
      const settled = this.usageFixtureRows().filter((r) => r.root !== null);
      const leaves = settled.map(usageLeafHash);
      this.usageTree = { settled, leaves, root: merkleRoot(leaves) };
    }
    return this.usageTree;
  }

  async usageProof(rowId: string): Promise<UsageProof> {
    const { settled, leaves, root } = this.usageTreeOf();
    const index = settled.findIndex((r) => r.id === rowId);
    if (index === -1) {
      // No such charge, or a charge that has not been committed yet: neither
      // has a proof to serve.
      throw new ApiError(404, 'not_found', `No inclusion proof for usage row ${rowId}`);
    }
    const r = settled[index];
    return {
      occurredAt: r.occurredAt,
      keyId: r.keyId,
      model: r.model,
      promptTokens: r.promptTokens,
      cachedTokens: r.cachedTokens,
      completionTokens: r.completionTokens,
      credits: r.credits,
      index,
      path: siblingPath(leaves, index),
      root,
    };
  }

  async funding(purchaseId: string): Promise<FundingStatus> {
    return {
      purchaseId,
      state: 'settled',
      ethTx: 'MockDepositSig111111111111111111111111111111111',
      ethBlock: 100,
      attestedHeight: 100,
      chainTip: 100,
      builderCeiling: 100,
      ccTx: 'MockMintSig11111111111111111111111111111111111',
      ccBlock: 50,
      credits: OPENING_BALANCE,
      amountUsd: 250,
      token: 'USDC',
      startedAt: '2026-08-22T20:00:00Z',
      updatedAt: '2026-08-22T20:00:00Z',
      chainKey: 0,
      chainName: 'Solana',
    };
  }

  async fundingList(): Promise<FundingStatus[]> {
    return this.fundingListData.map((s) => ({ ...s }));
  }

  /** The demo's payment source chain: Solana, the one chain Slotfare runs
   *  on, in exactly the wire shape `GET /console/chains` serves. */
  private readonly chainData: ChainInfo[] = [
    {
      chainKey: 0,
      name: 'Solana',
      chainId: 0,
      deposit: 'DemoTreasuryTok1111111111111111111111111111111',
      explorer: 'https://explorer.solana.com',
      rpc: 'https://api.devnet.solana.com',
    },
  ];

  async chains(): Promise<ChainInfo[]> {
    return this.chainData.map((c) => ({ ...c }));
  }

  /** The demo's attestation health for the Solana watcher: one healthy
   *  row. Served in exactly the wire shape the live route serves (envelope
   *  key `chains`), so the parity test can feed it through the same adapter
   *  the live path uses. */
  readonly attestEnvelope: { chains: AttestationChainWire[] } = {
    chains: [
      { chainKey: 0, name: 'Solana', known: true, attestedTip: 1_000_000, head: 1_000_001, lagBlocks: 1, lagSeconds: 4 },
    ],
  };

  async attestation(): Promise<AttestChain[]> {
    return this.attestEnvelope.chains.map(fromAttestationWire);
  }

  /** The demo's model catalog: six models across five vendors plus one
   *  unattributed, three kinds, three serving paths and every context
   *  bucket, each with its own per-million credit pricing. Served in
   *  exactly the wire shape the live route serves (envelope key `models`),
   *  so the parity test can feed it through the same adapter the live path
   *  uses. */
  readonly modelEnvelope: { models: ModelWire[]; baseUrl?: unknown } = {
    models: [
      { id: 'mistral-nemotron', name: 'Mistral Nemotron', type: 'chat', vendor: 'Mistral', contextLength: 131072, capabilities: ['function-calling'], description: 'A 12B reasoning model with strong tool use.', pricing: { uncachedInput: 20, cachedInput: 2, output: 30 } },
      { id: 'diffusiongemma-26b', name: 'Google DiffusionGemma 26B A4B', type: 'chat', vendor: 'Google', contextLength: 32768, servingPath: 'priority', description: 'Fast 26B mixture-of-experts chat model.', pricing: { uncachedInput: 50, cachedInput: 5, output: 250 } },
      { id: 'llama-3.2-3b', name: 'Llama 3.2 3B', type: 'chat', vendor: 'Meta', contextLength: 32768, parameters: '3B', servingPath: 'fast', capabilities: ['vision'], pricing: { uncachedInput: 10, cachedInput: 1, output: 40 } },
      { id: 'qwen3-8-27b', name: 'Qwen3 27B', type: 'chat', vendor: 'Alibaba', contextLength: 262144, parameters: '27B', servingPath: 'standard', capabilities: ['function-calling'], pricing: { uncachedInput: 100, cachedInput: 10, output: 300 } },
      { id: 'jina-v3-embeddings', name: 'Jina v3 Embeddings', type: 'embeddings', vendor: 'Jina', contextLength: 524288, description: 'Multilingual text embeddings.', pricing: { uncachedInput: 50 } },
      { id: 'rerank-v1', name: 'Slotfare Reranker v1', type: 'reranker', pricing: { uncachedInput: 20 } },
    ],
  };

  async models(): Promise<ModelList> {
    return {
      models: this.modelEnvelope.models.map(fromModelWire),
      // The demo wire advertises nothing, so the page-origin fallback stays.
      baseUrl:
        typeof this.modelEnvelope.baseUrl === 'string' &&
        this.modelEnvelope.baseUrl !== ''
          ? this.modelEnvelope.baseUrl
          : '',
    };
  }
}

// ---- provider -----------------------------------------------------------

/**
 * Provide the `SlotfareApi` singleton. When `environment.useMock` is true
 * the mock serves the demo fixtures; otherwise the HTTP client talks to
 * the live backend. Components never inject `SlotfareApi` directly. Only
 * the store does (enforced in T04).
 */
export function provideSlotfareApi(): EnvironmentProviders {
  if (environment.useMock) {
    return makeEnvironmentProviders([
      { provide: SLOTFARE_API, useClass: MockSlotfareApi },
    ]);
  }
  // Live mode: fetch-backed client so no XHR polyfill is needed.
  return makeEnvironmentProviders([
    provideHttpClient(withFetch()),
    { provide: SLOTFARE_API, useClass: HttpSlotfareApi },
  ]);
}
