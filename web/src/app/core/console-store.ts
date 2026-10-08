import {
  Service,
  computed,
  effect,
  inject,
  signal,
} from '@angular/core';
import { Subject } from 'rxjs';
import {
  CREDITS_PER_USD,
  clock,
  costOf,
  creditText,
  creditText1,
  usd,
} from './pricing';
import { ApiKey, Model, UsageRow, Wallet } from './models';
import { WALLET_ADDRESS } from './mock-data';
import { DAY_MS, burnPerDay, creditsPerHour, runwayDays, spendLast24h } from './burn';
import {
  buildSteps,
  solTxUrl,
  isDirectPurchase,
  isSettled,
  short,
  stageIndex,
  traceNote,
} from './trace';
import { WalletService } from './wallet';
import { SellService } from './sell';
import { BuyService } from './buy';
import { DepositService } from './deposit';
import { listingPrice } from './listing-price';
import { listingStatusLabel } from './role-label';
import { verifyUsageProof } from './commitment';
import {
  ApiError,
  AttestChain,
  ChainInfo,
  CreatedKey,
  SLOTFARE_API,
  FundingStatus,
  Key,
  MICRO_CREDITS_PER_CREDIT,
  UsageQuery,
  UsageRowT,
  UsageWire,
  UsageFacets,
  fromUsageWire,
} from './api';
import { environment } from '../../environments/environment';
import { API_BASE_URL } from './runtime-config';
import { Listing, MarketListing } from './models';
import { PollingService } from './polling';

/**
 * Wire unit: usage rows and the account's ledger figures both carry integer
 * micro-credits on the wire; the store divides them back down to credits.
 */

/** In-memory bound for the usage log; newer rows evict the oldest. */
const USAGE_CAP = 200;

/** The request log's fixed page size: one numbered page of the paged read. */
const LOG_PAGE_SIZE = 25;

/**
 * Merge a newer batch over already-loaded older pages, deduping by key. The
 * newest batch's order is kept first; any previously loaded row whose key is
 * not in the batch is appended after it, so a refresh never drops the older
 * pages and a load-more never duplicates a boundary row.
 */
function mergeById<T>(newest: T[], older: T[], key: (t: T) => string): T[] {
  const out = [...newest];
  const seen = new Set(out.map(key));
  for (const r of older) {
    const k = key(r);
    if (!seen.has(k)) {
      seen.add(k);
      out.push(r);
    }
  }
  return out;
}

/**
 * Bounded number of lock-confirmation reloads after a sell listing or
 * cancel confirms. The market feeder mirrors the on-chain state on an
 * interval measured in seconds, so the wait is short but not zero; if the
 * figure has not moved after this many poll ticks, we stop and let the
 * ordinary signed-in cycles deliver it.
 */
const LISTING_CONFIRM_MAX = 3;

/** Metering stream connection state. */
export type StreamState = 'off' | 'disconnected' | 'connected';

/** The Transactions view's filter, applied client-side over loaded data. */
export type TransactionFilter = 'all' | 'buying' | 'selling';
export type UsageStatusFilter = 'all' | 'metered' | 'error';

/** One merged row of the Transactions view: a deposit, a market buy or a
 *  sell order. `at` is the row's own timestamp; `destination` is where the
 *  row opens: a router path for deposits/sells, an external explorer link
 *  for buys (the D56 chain-driven rule). */
export interface TransactionRow {
  id: string;
  kind: 'deposit' | 'buy' | 'sell';
  at: string;
  statusText: string;
  credits: number | null;
  priceText: string | null;
  /** The network the row's money moved on: the source chain name for
   *  deposits, 'Solana' for market rows. */
  chainName: string;
  destination:
    | { kind: 'route'; path: [string, ...string[]] }
    | { kind: 'external'; href: string; label: string };
}

/** The server stores no plaintext for existing keys, so the list shows a
 *  stable id-based handle; the full key is only ever shown in the create
 *  modal. */
function keyDisplay(id: string): string {
  return `slf_${id}`;
}

/** The client-side inclusion-proof check of one request log row. */
export interface ProofCheck {
  state: 'checking' | 'verified' | 'failed' | 'error';
  /** failed: which check did not agree; error: the proof read's message. */
  detail?: string;
}

/**
 * The overview card for a market buy. Until the backend lists the buying
 * row the card is client-side state (design decision 2: market mints are not
 * funding rows on the wire); once the buying listing row lands it flips to
 * complete. The row's own detail is the one atomic transaction, so the card
 * links straight to the explorer.
 */
export interface BuyCardView {
  complete: boolean;
  txHash: string;
  credits: number;
  priceText: string;
  note: string;
  href: string;
  label: string;
}

/** The shape the template reads for the purchase card. */
interface PurchaseView {
  amount: number;
  token: string;
  credits: number;
  startedAt: string;
  ccBlock: number;
  /** The source chain's display name for the latest-funding badge. */
  chainName: string;
  /** The Paid line's primary figure: what was actually spent. */
  paidText: string;
  /** The Paid line's secondary figure; null for every current flow (kept
   *  as the slot a second line would use). */
  paidSub: string | null;
}

/**
 * The inference base URL the page itself serves: the current origin with
 * the documented `/v1` path. The fallback when the wire advertises none,
 * so no fabricated host ever renders. Guarded so non-DOM contexts never
 * throw.
 */
function pageOriginBase(): string {
  const origin =
    typeof window !== 'undefined' && window.location?.origin
      ? window.location.origin
      : '';
  return origin + '/v1';
}

/**
 * Every piece of console state, as signals. The store is the only thing
 * that talks to the API: balance, keys and usage each load through
 * `SlotfareApi` with a loading and an error signal alongside, so a screen
 * can show stale-with-warning rather than blank when a refresh fails.
 */
@Service()
export class ConsoleStore {
  private readonly walletSvc = inject(WalletService);
  private readonly api = inject(SLOTFARE_API);
  private readonly sell = inject(SellService);
  private readonly buy = inject(BuyService);
  private readonly deposit = inject(DepositService);
  /** The console-wide polling loop; drives every live figure on one cadence. */
  private readonly polling = inject(PollingService);
  /** Console API base; runtime config wins over the compiled default. */
  private readonly baseUrl = inject(API_BASE_URL);

  // ---- session -------------------------------------------------------
  readonly wallet = signal<Wallet | null>(null);
  /**
   * The real wallet address once connected; the mock constant keeps the
   * demo screens populated before a wallet is attached.
   */
  readonly address = computed(() => this.walletSvc.address() ?? WALLET_ADDRESS);
  /** Credit account id derived from the wallet address. */
  readonly accountId = computed(
    () => this.walletSvc.accountId() ?? '0x…',
  );
  readonly connected = computed(() => this.wallet() !== null);
  /** Connected and the sign-in verified: the only state the shell serves. */
  readonly signedIn = computed(() => this.wallet() !== null && this.walletSvc.session() !== null);
  /**
   * True while a 401 has dropped the session and the app is asking for a
   * fresh wallet signature. The shell renders its in-place re-sign prompt
   * from this; concurrent 401s collapse into a single prompt.
   */
  readonly sessionExpired = signal(false);

  // ---- balance -------------------------------------------------------
  /** Whole credits; empty until the account loads. Liveness lives in the
   *  loading/error signals below. */
  readonly balance = signal(0);
  /** Whole credits locked by open sell listings: shown beside the
   *  spendable headline, never silently subtracted from it. */
  readonly locked = signal(0);
  readonly balanceLoading = signal(false);
  /** Skeleton gate: loading AND no data yet, so a background refresh never
   *  tears rendered values down. */
  readonly balanceInitial = computed(
    () => this.balanceLoading() && !this.balanceHasData(),
  );
  /** True from the first successful read until the session drops; makes
   *  `balanceInitial` true only before data exists. */
  private readonly balanceHasData = signal(false);
  readonly balanceError = signal<string | null>(null);
  /** The inference base URL the account read advertised; '' until it does.
   *  Reset on session drop so a fresh sign-in re-reads it. */
  readonly advertisedBaseUrl = signal('');
  /**
   * The keys screen's base URL: the wire-advertised value when present,
   * otherwise the page's own origin with the documented `/v1` path. Never
   * a fabricated host.
   */
  readonly endpointBaseUrl = computed(() =>
    this.advertisedBaseUrl() !== '' ? this.advertisedBaseUrl() : pageOriginBase(),
  );
  readonly balanceUsd = computed(() => usd(this.balance()));
  readonly balanceText = computed(() => creditText1(this.balance()));
  readonly lockedText = computed(() => creditText1(this.locked()));
  readonly lockedUsd = computed(() => usd(this.locked()));

  // ---- purchase being configured -------------------------------------
  readonly amount = signal(250);
  readonly creditsToBuy = computed(() => this.amount() * CREDITS_PER_USD);
  /** The payment line: the configured amount in USDC, the market's one
   *  token (the deposit target serves the mint at runtime). */
  readonly payText = computed(() => `${this.amount().toFixed(2)} USDC`);
  readonly amountValid = computed(
    () => this.amount() >= 5 && this.amount() <= 50_000,
  );

  // ---- payment source chains -----------------------------------------
  /** Every chain the backend reports; key 0 is the Solana row - the only
   *  row a Slotfare deployment serves. */
  readonly chains = signal<ChainInfo[]>([]);
  readonly chainsLoading = signal(false);
  /** Skeleton gate: loading AND no data yet, so a background refresh never
   *  tears rendered values down. */
  readonly chainsInitial = computed(
    () => this.chainsLoading() && !this.chainsHasData(),
  );
  /** True from the first successful read until the session drops; makes
   *  `chainsInitial` true only before data exists. */
  private readonly chainsHasData = signal(false);
  readonly chainsError = signal<string | null>(null);
  /**
   * The explorer base for the trace's source chain, looked up from the
   * loaded chains by the purchase's chain key. Empty when unknown, which
   * leaves the eth-lane links blank rather than pointing at a hardcoded host.
   */
  readonly traceSourceExplorer = computed(() => {
    const s = this.fundingStatus();
    if (!s) return '';
    return this.chains().find((c) => c.chainKey === s.chainKey)?.explorer ?? '';
  });

  // ---- attestation health ---------------------------------------------
  /** Every chain's attestation health; empty when nothing has been scanned
   *  yet (the card hides itself). */
  readonly attestation = signal<AttestChain[]>([]);
  readonly attestationLoading = signal(false);
  /** Skeleton gate: loading AND no data yet, so a background refresh never
   *  tears rendered values down. */
  readonly attestationInitial = computed(
    () => this.attestationLoading() && !this.attestationHasData(),
  );
  /** True from the first successful read until the session drops; makes
   *  `attestationInitial` true only before data exists. */
  private readonly attestationHasData = signal(false);
  readonly attestationError = signal<string | null>(null);

  /** Refresh the payment source chains from the API. */
  async loadChains(): Promise<void> {
    this.chainsLoading.set(true);
    try {
      const list = await this.api.chains();
      this.chains.set(list);
      this.chainsError.set(null);
      this.chainsHasData.set(true);
    } catch (e) {
      this.handleLoadError((m) => this.chainsError.set(m), e);
    } finally {
      this.chainsLoading.set(false);
    }
  }

  /** Refresh the source chains' attestation health from the API. Rides the
   *  shared 15 s loop: the server caches each row ~15 s, so the cadence is
   *  safe. On failure the previous list stays in place. */
  async loadAttestation(): Promise<void> {
    this.attestationLoading.set(true);
    try {
      const list = await this.api.attestation();
      this.attestation.set(list);
      this.attestationError.set(null);
      this.attestationHasData.set(true);
    } catch (e) {
      this.handleLoadError((m) => this.attestationError.set(m), e);
    } finally {
      this.attestationLoading.set(false);
    }
  }

  // ---- model catalog ---------------------------------------------------
  /** Every model the endpoint serves, with its per-million credit pricing. */
  readonly models = signal<Model[]>([]);
  readonly modelsLoading = signal(false);
  /** Skeleton gate: loading AND no data yet, so a background refresh never
   *  tears rendered values down. */
  readonly modelsInitial = computed(
    () => this.modelsLoading() && !this.modelsHasData(),
  );
  /** True from the first successful read until the session drops; makes
   *  `modelsInitial` true only before data exists. */
  private readonly modelsHasData = signal(false);
  readonly modelsError = signal<string | null>(null);
  /** The id of the selected model; null until one is picked. */
  readonly selectedModelId = signal<string | null>(null);
  /**
   * The selected model. Falls back to the first row when nothing is picked
   * or the pick is no longer in the list, so the endpoint screen always has
   * a model to price against. Null when no catalog has loaded.
   */
  readonly selectedModel = computed<Model | null>(() => {
    const list = this.models();
    return list.find((m) => m.id === this.selectedModelId()) ?? list[0] ?? null;
  });
  /** The cheapest model by uncached input price; null when the catalog is empty. */
  readonly cheapestModel = computed<Model | null>(() => {
    const list = this.models();
    if (list.length === 0) return null;
    return list.reduce((a, b) => (b.pricing.uncachedInput < a.pricing.uncachedInput ? b : a));
  });

  /** Choose which model the keys screen's example prices against. */
  setModel(id: string): void {
    this.selectedModelId.set(id);
  }

  /** Refresh the model catalog from the API. On failure the previous list
   *  stays in place, so an empty list after a good one degrades in place. */
  async loadModels(): Promise<void> {
    this.modelsLoading.set(true);
    try {
      const { models: list, baseUrl } = await this.api.models();
      // The endpoint advertises its inference base URL on this payload; map
      // it in only when present so a wire that carries none leaves the
      // signal (and the page-origin fallback) untouched.
      if (baseUrl !== '') {
        this.advertisedBaseUrl.set(baseUrl);
      }
      this.models.set(list);
      this.modelsError.set(null);
      this.modelsHasData.set(true);
      // Keep the pick valid: if it is unset or no longer present, use the
      // first row so the keys screen always has a model to price against.
      const id = this.selectedModelId();
      if (id === null || !list.some((m) => m.id === id)) {
        this.selectedModelId.set(list[0]?.id ?? null);
      }
    } catch (e) {
      this.handleLoadError((m) => this.modelsError.set(m), e);
    } finally {
      this.modelsLoading.set(false);
    }
  }

  // ---- funding trace --------------------------------------------------
  /** The live funding status polled from the backend. */
  readonly fundingStatus = signal<FundingStatus | null>(null);
  /**
   * Wall-clock start of each direct purchase this browser sent,
   * keyed by its mint transaction. The server stamps a direct deposit with
   * start = credited = block time, so without this the finished trace would
   * read 00:00; the wait the user actually saw runs from the wallet returning
   * to the mint being indexed.
   */
  private readonly localStarts = new Map<string, string>();
  /** True until the first poll has returned; drives the trace skeleton. */
  readonly fundingLoading = signal(true);
  /** Skeleton gate: loading AND no data yet, so a background refresh never
   *  tears rendered values down. */
  readonly fundingInitial = computed(
    () => this.fundingLoading() && !this.fundingHasData(),
  );
  /** True from the first successful read until the session drops; makes
   *  `fundingInitial` true only before data exists. */
  private readonly fundingHasData = signal(false);
  /** Last poll failure, surfaced so a stale figure never looks live. */
  readonly fundingError = signal<string | null>(null);
  /** Every purchase the backend knows for this account, newest first. */
  readonly fundingHistory = signal<FundingStatus[]>([]);
  /** The purchase the user picked from the history; null follows the
   *  default (newest in flight, else newest). */
  readonly selectedPurchaseId = signal<string | null>(null);
  /**
   * A payment this browser just sent, until the backend has seen it on
   * chain. The backend only records mined deposits, so without this the
   * seconds between "sent" and "mined + scanned" would show nothing.
   */
  private readonly localPending = signal<FundingStatus | null>(null);
  /** Wall clock, advanced once a second while polling so the elapsed time
   *  of an in-flight purchase counts live instead of jumping per poll. */
  private readonly now = signal(Date.now());
  private clockTimer: ReturnType<typeof setInterval> | undefined;

  /** Purchase view the template reads (amount, token, credits, etc.). */
  readonly purchase = computed<PurchaseView>(() => {
    const s = this.fundingStatus();
    if (s === null) {
      // No in-flight purchase: show the configured amount/token.
      return {
        amount: this.amount(),
        token: 'USDC',
        credits: this.creditsToBuy(),
        startedAt: '-',
        ccBlock: 0,
        chainName: '',
        paidText: this.payText(),
        paidSub: null,
      };
    }
    // The deposit fixes both figures the moment it is seen: credits mint at
    // 1,000 per dollar of the deposit amount, deterministically. Until the
    // server's minted credits arrive they are derived here; when they do
    // they take over - the same number.
    return {
      amount: s.amountUsd,
      token: s.token,
      credits: s.credits ?? Math.round(s.amountUsd * CREDITS_PER_USD),
      startedAt: s.startedAt,
      ccBlock: s.ccBlock ?? 0,
      chainName: s.chainName,
      paidText: `${s.amountUsd.toFixed(2)} ${s.token}`,
      paidSub: null,
    };
  });

  readonly steps = computed(() =>
    this.fundingStatus() !== null
      ? buildSteps(this.fundingStatus()!, this.traceSourceExplorer())
      : [],
  );

  readonly traceComplete = computed(() =>
    this.fundingStatus() !== null && isSettled(this.fundingStatus()!),
  );

  readonly stepLabel = computed(() => {
    const s = this.fundingStatus();
    if (s === null) return 'No purchase in flight';
    if (isSettled(s)) return 'Complete';
    const total = isDirectPurchase(s) ? 1 : 3;
    return `Step ${stageIndex(s) + 1} of ${total}`;
  });

  /** Time from the payment to now while proving; from the payment to the
   *  credit once settled, so a finished trace shows how long it took. */
  readonly elapsedText = computed(() => {
    const s = this.fundingStatus();
    if (s === null) return '-';
    const start = new Date(s.startedAt).getTime();
    if (Number.isNaN(start)) return '-';
    const end = isSettled(s) ? new Date(s.updatedAt).getTime() : this.now();
    if (Number.isNaN(end)) return '-';
    // A settled direct purchase whose start this browser never saw has
    // start = credited (block time); 00:00 would claim it was instant.
    if (isSettled(s) && isDirectPurchase(s) && end <= start) return '-';
    const elapsedSec = Math.max(0, (end - start) / 1000);
    return clock(elapsedSec);
  });

  readonly etaText = computed(() =>
    this.traceComplete()
      ? 'Credits available now'
      : 'Proving payment…',
  );

  readonly traceNoteText = computed(() => {
    const s = this.fundingStatus();
    if (s === null) return '';
    return traceNote(s);
  });

  /** Per-segment fill for the progress bar: three segments for a proven
   *  purchase, one for a direct. */
  readonly segments = computed(() => {
    const steps = this.steps();
    if (steps.length === 0) {
      return Array.from({ length: 3 }, () => ({ state: 'pending' as const, percent: 0 }));
    }
    return steps.map((s) => ({
      state: s.state,
      percent: s.state === 'done' ? 100 : s.state === 'live' ? 100 : 0,
    }));
  });

  // ---- listings (sell) -----------------------------------------------
  /** Every market row this account owns, every state, newest first. */
  readonly listings = signal<Listing[]>([]);
  readonly listingsLoading = signal(false);
  /** Skeleton gate: loading AND no data yet, so a background refresh never
   *  tears rendered values down. */
  readonly listingsInitial = computed(
    () => this.listingsLoading() && !this.listingsHasData(),
  );
  /** True from the first successful read until the session drops; makes
   *  `listingsInitial` true only before data exists. */
  private readonly listingsHasData = signal(false);
  readonly listingsError = signal<string | null>(null);
  /**
   * A listing this browser just sent, until the backend lists it. Its id is
   * the listing transaction hash (the D57 stand-in, exactly like
   * `localPending` for purchases); the backend's row carries the same hash
   * as `listedTx` plus its own numeric listing id. The lock must be visible
   * the moment the wallet returns, not a feeder pass later.
   */
  private readonly localListing = signal<Listing | null>(null);
  /** The id of the locally pending row, or null when none: that row has no
   *  on-chain listing id yet, so it offers no Cancel. */
  readonly pendingListingId = computed(() => this.localListing()?.id ?? null);
  /**
   * The account's open sell listings; they are what holds the lock.
   * Buying rows are excluded: a purchased listing locks nothing of the
   * buyer's credits, so it never counts toward the open-sell figure.
   */
  readonly openListings = computed(() => this.listings().filter((l) => l.role === 'selling' && l.state === 'open'));
  readonly openListingsCount = computed(() => this.openListings().length);
  /**
   * The account's sell-side rows, every state: what the Sell screen's
   * listing management shows. A bought listing is not one of my listings,
   * so it never appears here.
   */
  readonly sellListings = computed(() => this.listings().filter((l) => l.role === 'selling'));

  // ---- market (buy) --------------------------------------------------
  /** Open listings on the market from any seller, newest first, as read
   *  from the console API (Design decision 1: no chain reads). */
  readonly market = signal<MarketListing[]>([]);
  readonly marketLoading = signal(false);
  /** Skeleton gate: loading AND no data yet, so a background refresh never
   *  tears rendered values down. */
  readonly marketInitial = computed(
    () => this.marketLoading() && !this.marketHasData(),
  );
  /** True from the first successful read until the session drops; makes
   *  `marketInitial` true only before data exists. */
  private readonly marketHasData = signal(false);
  readonly marketError = signal<string | null>(null);
  /**
   * Every open listing on the market, the viewer's own included: it is
   * for sale, whatever else it is to its owner.
   */
  readonly forSaleCount = computed(() => this.market().length);
  /** Count of not-mine, matured, open listings; drives the shell subtitle. */
  readonly buyableCount = computed(
    () => this.market().filter((l) => !l.mine && l.matured).length,
  );
  /**
   * The complete buy card for the overview: client-side state only (design
   * decision 2 - market sales are not funding rows on the wire), published
   * the moment the settlement call returns. The row's own detail is the
   * same USDC transfer, so the card links straight to the explorer.
   */
  private readonly buySnap = signal<{
    txHash: string;
    credits: number;
    priceText: string;
    startedAt: string;
  } | null>(null);
  /** The buy card for the overview; null when idle. */
  readonly buyCard = computed<BuyCardView | null>(() => {
    const snap = this.buySnap();
    if (snap === null) return null;
    return {
      complete: true,
      txHash: snap.txHash,
      credits: snap.credits,
      priceText: snap.priceText,
      note: 'Paid and credited. Your balance now includes these credits.',
      href: solTxUrl(snap.txHash),
      label: 'Solana explorer \u00b7 transaction',
    };
  });
  /** Single timer for the post-sell locked-figure confirmation loop. */
  private listingConfirmTimer: ReturnType<typeof setInterval> | undefined;

  // ---- transactions (merged view) ------------------------------------
  /** The Transactions view filter: 'all', 'buying', or 'selling'. */
  readonly transactionFilter = signal<TransactionFilter>('all');
  /**
   * The one merged list: deposits (funding history) plus market rows
   * (listings, both roles), newest first by each row's own timestamp.
   * A missing/empty timestamp sorts last.
   */
  readonly transactions = computed<TransactionRow[]>(() => {
    const depositRows = this.fundingHistory().map<TransactionRow>((p) => ({
      id: p.purchaseId,
      kind: 'deposit',
      at: p.startedAt,
      statusText:
        p.state === 'settled' ? 'Settled'
        : p.state === 'confirmed' ? 'Proving'
        : p.state === 'pending' ? 'Pending'
        : 'Failed',
      credits: p.credits,
      priceText: p.amountUsd > 0 ? `${p.amountUsd.toFixed(2)} ${p.token}` : null,
      chainName: p.chainName,
      destination: { kind: 'route', path: ['/funding', p.purchaseId] },
    }));
    const marketRows = this.listings().map<TransactionRow>((l) => {
      const sell = l.role === 'selling';
      const statusText = listingStatusLabel(l.role, l.state, l.matured);
      const lp = listingPrice(l);
      // A finished trade (sold or cancelled) opens the detail view; an open
      // selling row stays on the Sell screen for management, and an open
      // buying row keeps its external explorer link (the in-flight case).
      let destination: TransactionRow['destination'];
      if (l.state === 'open') {
        destination = sell
          ? ({ kind: 'route', path: ['/sell'] } as const)
          : ({
              kind: 'external',
              href: solTxUrl(l.listedTx),
              label: 'Solana explorer · transaction',
            } as const);
      } else {
        destination = { kind: 'route', path: ['/trade', l.id] } as const;
      }
      return {
        id: l.id,
        kind: (sell ? 'sell' : 'buy') as 'buy' | 'sell',
        // A buy happened when the sale landed, not when the seller listed;
        // a sell row keeps the listing time, the moment its credits locked.
        at: sell ? l.createdAt : (l.soldAt ?? l.createdAt),
        statusText,
        credits: l.credits,
        priceText: `${lp.text} ${lp.label}`,
        chainName: 'Solana',
        destination,
      };
    });
    const all = [...depositRows, ...marketRows];
    all.sort((a, b) => {
      const ta = a.at, tb = b.at;
      if (ta === tb) return 0;
      if (ta === '') return 1;
      if (tb === '') return -1;
      return ta < tb ? 1 : -1;
    });
    return all;
  });
  /** The merged list narrowed by the active filter. */
  readonly filteredTransactions = computed<TransactionRow[]>(() => {
    const rows = this.transactions();
    switch (this.transactionFilter()) {
      case 'buying':
        return rows.filter((r) => r.kind === 'deposit' || r.kind === 'buy');
      case 'selling':
        return rows.filter((r) => r.kind === 'sell');
      default:
        return rows;
    }
  });
  /** Count of rows currently shown, for the shell subtitle. */
  readonly transactionCount = computed(
    () => this.filteredTransactions().length,
  );

  /** Set the Transactions filter. Client-side only: no request. */
  setTransactionFilter(f: TransactionFilter): void {
    this.transactionFilter.set(f);
  }

  /**
   * The before-id cursor after the oldest listing row loaded so far; null
   * when the history is exhausted. A non-null cursor is what shows the
   * Transactions screen's "Load more" control. It is a decimal string: a
   * listing id is a full uint64 and a JS number rounds above 2^53.
   */
  readonly listingsCursor = signal<string | null>(null);
  /** A load-more is in flight; the control disables and re-entry is blocked. */
  readonly loadingMoreTransactions = signal(false);
  /** True while the market rows can load more older rows. */
  readonly transactionsHasMore = computed(
    () => this.listingsCursor() !== null,
  );

  /** Load the next page of older market rows under the current cursor and
   *  append them in place (no page swap). Deposits need no cursor: the
   *  funding read returns every purchase on each poll. A no-op when nothing
   *  is loaded yet, the history is exhausted, or a load is in flight. */
  async loadMoreTransactions(): Promise<void> {
    const cursor = this.listingsCursor();
    if (cursor === null || this.loadingMoreTransactions()) return;
    this.loadingMoreTransactions.set(true);
    try {
      const page = await this.api.listings(cursor);
      this.listings.update((rows) => mergeById(rows, page.rows, (l) => l.id));
      this.listingsCursor.set(page.nextCursor);
      this.listingsHasData.set(true);
    } catch (e) {
      this.handleLoadError((m) => this.listingsError.set(m), e);
    } finally {
      this.loadingMoreTransactions.set(false);
    }
  }

  // ---- api keys -------------------------------------------------------
  readonly keys = signal<ApiKey[]>([]);
  readonly keysLoading = signal(false);
  /** Skeleton gate: loading AND no data yet, so a background refresh never
   *  tears rendered values down. */
  readonly keysInitial = computed(
    () => this.keysLoading() && !this.keysHasData(),
  );
  /** True from the first successful read until the session drops; makes
   *  `keysInitial` true only before data exists. */
  private readonly keysHasData = signal(false);
  readonly keysError = signal<string | null>(null);
  readonly activeKeys = computed(() => this.keys().filter((k) => !k.revoked));

  // ---- usage ----------------------------------------------------------
  readonly usage = signal<UsageRow[]>([]);
  readonly usageLoading = signal(false);
  /** Skeleton gate: loading AND no data yet, so a background refresh never
   *  tears rendered values down. */
  readonly usageInitial = computed(
    () => this.usageLoading() && !this.usageHasData(),
  );
  /** True from the first successful read until the session drops; makes
   *  `usageInitial` true only before data exists. */
  private readonly usageHasData = signal(false);
  readonly usageError = signal<string | null>(null);
  /**
   * The status chip: 'all', 'error', or 'metered'. Error and All ride the
   * server query (errors=true / no param); Metered has no server-side
   * counterpart, so it narrows the loaded page client-side instead.
   */
  readonly usageStatusFilter = signal<UsageStatusFilter>('all');
  /** Chart range in days; drives what the chart plots, not the log. */
  readonly usageDays = signal<number>(7);

  // ---- request-log structured filters ----------------------------------
  /** The key prefix filter; '' is "All" (no prefix sent). */
  readonly usageKey = signal<string>('');
  /** The model id filter; '' is "All" (no model sent). */
  readonly usageModel = signal<string>('');
  /** Start of the date range in unix seconds, inclusive; 0 means unset. */
  readonly usageFrom = signal<number>(0);
  /** End of the date range in unix seconds, inclusive; 0 means unset. */
  readonly usageTo = signal<number>(0);
  /** The current page of the numbered read, 1-based. */
  readonly usagePage = signal<number>(1);
  /** The count of rows matching the active filters, as reported by the wire. */
  readonly usageTotal = signal<number>(0);
  /** The account's distinct key prefixes and model ids, for the dropdowns. */
  readonly usageFacets = signal<UsageFacets>({ prefixes: [], models: [] });
  /** Bumped on every load so a slow response never overwrites a newer one. */
  private usageLoadSeq = 0;

  /** Number of pages the matching total spans at the fixed page size. */
  readonly usageTotalPages = computed(() =>
    Math.max(1, Math.ceil(this.usageTotal() / LOG_PAGE_SIZE)),
  );

  /**
   * The loaded page narrowed by the client-side status filter. All and Error
   * are already server-narrowed, so they pass through untouched; only Metered
   * filters here, because the paged read has no "settled only" parameter.
   */
  readonly filteredUsage = computed<UsageRow[]>(() => {
    const rows = this.usage();
    if (this.usageStatusFilter() === 'metered') {
      return rows.filter((r) => r.error === null);
    }
    return rows;
  });

  readonly usageTotals = computed(() => {
    const rows = this.filteredUsage();
    const sum = (pick: (r: UsageRow) => number | null) =>
      rows.reduce((acc, r) => acc + (pick(r) ?? 0), 0);
    return {
      count: rows.length,
      prompt: sum((r) => r.prompt),
      cached: sum((r) => r.cached),
      completion: sum((r) => r.completion),
      credits: sum((r) => r.credits),
    };
  });

  readonly usageEmpty = computed(() => this.usage().length === 0);
  /** The rows on the current page after the status chip narrows them. */
  readonly usageCountText = computed(
    () => `${this.filteredUsage().length} requests`,
  );
  /**
   * The inclusion-proof check per request log row id: set when a verify is
   * in flight or has finished, absent for rows never checked.
   */
  readonly proofChecks = signal<Record<string, ProofCheck>>({});

  /** Set the status chip. All/Error change the server query, so they reset to
   *  page one and re-fetch; Metered has no server-side counterpart and only
   *  narrows the already-loaded page, so it issues no request. */
  setUsageFilter(f: UsageStatusFilter): void {
    if (f === this.usageStatusFilter()) return;
    this.usageStatusFilter.set(f);
    if (f !== 'metered') this.resetPageAndLoad();
  }

  /** Pick a key prefix ('' for All); resets to page one and re-fetches. */
  setUsageKey(prefix: string): void {
    this.usageKey.set(prefix);
    this.resetPageAndLoad();
  }

  /** Pick a model id ('' for All); resets to page one and re-fetches. */
  setUsageModel(id: string): void {
    this.usageModel.set(id);
    this.resetPageAndLoad();
  }

  /** Set the range start in unix seconds (0 clears it); resets to page one. */
  setUsageFrom(sec: number): void {
    this.usageFrom.set(sec);
    this.resetPageAndLoad();
  }

  /** Set the range end in unix seconds (0 clears it); resets to page one. */
  setUsageTo(sec: number): void {
    this.usageTo.set(sec);
    this.resetPageAndLoad();
  }

  /** Clear every structured filter and the status chip back to their defaults
   *  in one action, then reload page one. */
  clearUsageFilters(): void {
    this.usageStatusFilter.set('all');
    this.usageKey.set('');
    this.usageModel.set('');
    this.usageFrom.set(0);
    this.usageTo.set(0);
    this.resetPageAndLoad();
  }

  /** Move to a numbered page, clamped to the total; no-op when unchanged. */
  gotoPage(n: number): void {
    const target = Math.min(Math.max(1, n), this.usageTotalPages());
    if (target === this.usagePage()) return;
    this.usagePage.set(target);
    void this.loadUsage();
  }

  /** Reset to page one when not already there, then fetch the current page. */
  private resetPageAndLoad(): void {
    if (this.usagePage() !== 1) this.usagePage.set(1);
    void this.loadUsage();
  }

  /**
   * Verify one settled row's charge against its committed root: fetch the
   * inclusion proof and walk it client-side (recompute leaf, walk path,
   * compare root). No-op when the row has no commitment yet (awaiting
   * settlement). A failed read leaves the previous value in place, like
   * every other read in the store.
   */
  async verifyProof(row: UsageRow): Promise<void> {
    if (row.commitment === null) return;
    this.proofChecks.update((m) => ({ ...m, [row.id]: { state: 'checking' } }));
    try {
      const proof = await this.api.usageProof(row.id);
      if (verifyUsageProof(proof, row.commitment.root)) {
        this.proofChecks.update((m) => ({ ...m, [row.id]: { state: 'verified' } }));
      } else {
        this.proofChecks.update((m) => ({
          ...m,
          [row.id]: {
            state: 'failed',
            detail: 'Computed root does not match the committed root',
          },
        }));
      }
    } catch (e) {
      this.proofChecks.update((m) => ({
        ...m,
        [row.id]: {
          state: 'error',
          detail: e instanceof Error ? e.message : 'Proof unavailable',
        },
      }));
    }
  }

  // ---- derived spend figures ------------------------------------------
  // Each is a computed over the `usage`/`balance` signals, so it recomputes
  // when they change. The clock is read at evaluation time (never in a
  // template); time-based staleness between loads is acceptable because no
  // value is hardcoded.

  /** Credits spent across the last 24 hours of usage. */
  readonly spend24h = computed(() => spendLast24h(this.usage(), Date.now()));
  /** Credits per hour over the last 24 hours. */
  readonly creditsPerHour = computed(() => creditsPerHour(this.usage(), Date.now()));
  /** Seven-day mean burn rate; null when there is no usage in the window. */
  readonly burn7d = computed(() => burnPerDay(this.usage(), Date.now()));
  /** Days until the balance empties at the seven-day burn rate; null when undefined. */
  readonly runwayDays = computed(() => runwayDays(this.balance(), this.burn7d()));
  /** The calendar date the balance empties, e.g. "6 Sep"; null when runway is undefined. */
  readonly emptyDateText = computed<string | null>(() => {
    const days = this.runwayDays();
    if (days === null) return null;
    const t = new Date(Date.now() + days * DAY_MS);
    return new Intl.DateTimeFormat('en-US', {
      month: 'short',
      day: 'numeric',
    }).format(t);
  });

  /** The in-flight purchase id, or null when nothing is being funded. */
  readonly purchaseId = computed(() => this.fundingStatus()?.purchaseId ?? null);

  // ---- loading --------------------------------------------------------
  /**
   * Which account has been loaded. A 401 clears the session, which resets
   * this guard. Nothing reloads until the user signs in again, so an
   * expired token never retries forever.
   */
  private readonly loadedFor = signal<string | null>(null);

  constructor() {
    // Every tick of the shared loop refreshes all five live reads at once.
    // The store is a root singleton, so this subscription lives for the app
    // lifetime and no screen owns its own timer.
    this.polling.tick.subscribe(() => {
      void this.refreshAll();
    });

    // A page refresh restores a durable session silently: seed the wallet
    // chip from the restored entry so `signedIn` is true and the effect
    // below loads data with the stored token, without a new signature.
    // Tolerant of wallet services that predate the restored signals.
    if (this.wallet() === null && typeof this.walletSvc.restoredWallet === 'function') {
      const restored = this.walletSvc.restoredWallet();
      if (restored !== null) this.wallet.set(restored);
    }

    effect(() => {
      const s = this.walletSvc.session();
      if (s === null) {
        this.loadedFor.set(null);
        this.clearReadState();
        this.stopStream();
        this.stopPolling();
        return;
      }
      // A live session means recovery succeeded (or we never left): drop
      // the prompt so the shell returns to serving the current route.
      this.sessionExpired.set(false);
      if (this.loadedFor() === s.accountId) return;
      this.loadedFor.set(s.accountId);
      // One shared 15 s loop drives every live figure; its immediate refresh
      // covers account, usage, funding, listings and market exactly once each,
      // so no read is loaded twice on entry. Keys, chains and models are not
      // live figures: they load once here and refresh on their own mutations.
      this.startPolling();
      void this.loadKeys();
      void this.loadChains();
      void this.loadModels();
      this.startStream();
    });

    // When recovery is underway and the wallet publishes an error - the user
    // declined or the re-sign failed - abandon it: back to a clean sign-in
    // state with no stuck error cards.
    effect(() => {
      if (this.sessionExpired() && this.walletSvc.error() !== null) {
        this.abandonRecovery();
      }
    });
  }

  /** Refresh the FULL account read from the API: spendable balance and
   *  locked credits arrive in one response and are written together, so a
   *  refresh can never half-update the screen. */
  async loadAccount(): Promise<void> {
    this.balanceLoading.set(true);
    try {
      const a = await this.api.account();
      this.balance.set(a.balanceCredits);
      this.locked.set(a.lockedCredits);
      // Harmless fallback: honor an account-wire base URL when present, but
      // never clobber the value the models read advertised (this runs every
      // poll tick).
      if (a.baseUrl !== '') {
        this.advertisedBaseUrl.set(a.baseUrl);
      }
      this.balanceError.set(null);
      this.balanceHasData.set(true);
    } catch (e) {
      this.handleLoadError((m) => this.balanceError.set(m), e);
    } finally {
      this.balanceLoading.set(false);
    }
  }

  /** Refresh the account's listings from the API. The local-pending row
   *  stays at the top until the backend lists it under the same `listedTx`;
   *  the server's row then replaces it with its numeric listing id. Older
   *  pages already loaded by "Load more" are preserved: the fresh newest
   *  batch is merged over them, and the deeper of the two cursors wins so a
   *  poll never walks the cursor back up. */
  async loadListings(): Promise<void> {
    this.listingsLoading.set(true);
    try {
      const page = await this.api.listings();
      const server = page.rows;
      this.listingsCursor.set(this.deeperCursor(this.listingsCursor(), page.nextCursor));
      const local = this.localListing();
      let list = server;
      // Preserve older listing pages already loaded; the local row is
      // re-prepended below from its own signal, so drop it here.
      const prevServer = this.listings().filter((l) => local === null || l.id !== local.id);
      list = mergeById(server, prevServer, (l) => l.id);
      if (local !== null) {
        if (server.some((l) => l.listedTx !== '' && l.listedTx === local.listedTx)) {
          this.localListing.set(null);
        } else {
          list = [local, ...list];
        }
      }
      this.listings.set(list);
      this.listingsError.set(null);
      this.listingsHasData.set(true);
    } catch (e) {
      this.handleLoadError((m) => this.listingsError.set(m), e);
    } finally {
      this.listingsLoading.set(false);
    }
  }

  /** Refresh the open market listings from the API. The server returns them
   *  newest first with the authoritative `matured` boolean; `mine` passes
   *  straight through the adapter. There is no client-side ownership
   *  compare (accounts are hashed wallets; the server is the one that
   *  knows). No polling: a lost race is a normal outcome, so the screen
   *  refreshes on demand. */
  async loadMarket(): Promise<void> {
    this.marketLoading.set(true);
    try {
      const list = await this.api.market();
      this.market.set(list);
      this.marketError.set(null);
      this.marketHasData.set(true);
    } catch (e) {
      this.handleLoadError((m) => this.marketError.set(m), e);
    } finally {
      this.marketLoading.set(false);
    }
  }

  /**
   * After a confirmed sell (a listing or a cancel), re-read the full account
   * read (spendable and locked move together in one response) through the
   * same paths as any other credit on a short interval until `locked`
   * leaves `prevLocked`, bounded by `LISTING_CONFIRM_MAX`. The loop never
   * writes a single figure of its own; the ordinary poll's account re-read
   * is the backstop.
   */
  confirmLockedChange(prevLocked: number): void {
    this.stopListingConfirm();
    let attempts = 0;
    void this.loadAccount();
    this.listingConfirmTimer = setInterval(() => {
      if (this.locked() !== prevLocked) {
        this.stopListingConfirm();
        return;
      }
      attempts += 1;
      if (attempts >= LISTING_CONFIRM_MAX) {
        this.stopListingConfirm();
        return;
      }
      // The listings read keeps the sell screen's rows honest while the
      // mirror lags behind the account's locked figure.
      void this.loadAccount();
      void this.loadListings();
    }, 2_000);
  }

  /** Stop the post-sell locked-figure confirmation loop. Safe when idle. */
  stopListingConfirm(): void {
    if (this.listingConfirmTimer !== undefined) {
      clearInterval(this.listingConfirmTimer);
      this.listingConfirmTimer = undefined;
    }
  }

  /** Refresh the key list from the API. */
  async loadKeys(): Promise<void> {
    this.keysLoading.set(true);
    try {
      const list = await this.api.keys();
      this.keys.set(list.map((k) => this.toApiKey(k)));
      this.keysError.set(null);
      this.keysHasData.set(true);
    } catch (e) {
      this.handleLoadError((m) => this.keysError.set(m), e);
    } finally {
      this.keysLoading.set(false);
    }
  }

  /**
   * Refresh the request log from the paged read, applying the active
   * structured filters and the current page. The page, the matching total and
   * the account's distinct facets all arrive in one response; a stale
   * response (a newer load has since started) is dropped so a slow fetch can
   * never overwrite a fresher one.
   */
  async loadUsage(): Promise<void> {
    this.usageLoading.set(true);
    const seq = ++this.usageLoadSeq;
    try {
      const q: UsageQuery = {
        page: this.usagePage(),
        limit: LOG_PAGE_SIZE,
        key: this.usageKey() || undefined,
        model: this.usageModel() || undefined,
        from: this.usageFrom() || undefined,
        to: this.usageTo() || undefined,
        errors: this.usageStatusFilter() === 'error',
      };
      const page = await this.api.usage(q);
      if (seq !== this.usageLoadSeq) return;
      this.usage.set(page.rows.map((r) => this.toUsageRow(r)));
      this.usageTotal.set(page.total ?? 0);
      this.usageFacets.set(
        page.facets ?? { prefixes: [], models: [] },
      );
      this.usageError.set(null);
      this.usageHasData.set(true);
    } catch (e) {
      if (seq !== this.usageLoadSeq) return;
      this.handleLoadError((m) => this.usageError.set(m), e);
    } finally {
      if (seq === this.usageLoadSeq) this.usageLoading.set(false);
    }
  }

  /**
   * Keep the deeper (smaller) of two before-id cursors: ids grow with
   * recency, so the smaller one reaches further into the loaded history.
   * A null fresh cursor means the batch already reached the oldest row, so
   * nothing is left beyond it.
   */
  private deeperCursor(current: string | null, fresh: string | null): string | null {
    if (fresh === null) return null;
    if (current === null) return fresh;
    return BigInt(current) <= BigInt(fresh) ? current : fresh;
  }

  /**
   * Central failure path for the loaders. A 401 drops the session rather
   * than retrying; anything else leaves the previous value in place and
   * publishes the message, so a screen shows stale-with-warning.
   */
  private handleLoadError(
    setError: (m: string | null) => void,
    e: unknown,
  ): void {
    if (e instanceof ApiError && e.status === 401) {
      // One collapsed recovery flow, not a per-card error: the app asks for
      // a fresh signature in place. No card ever shows its own expired text.
      this.beginSessionRecovery();
      return;
    }
    setError(e instanceof Error ? e.message : 'Something went wrong');
  }

  /**
   * Enter the single session-recovery flow after any 401. Idempotent: once
   * armed, further concurrent 401s are no-ops, so parallel reads produce
   * one prompt, not a storm. Stops every timer, drops the session (and its
   * durable copy), and clears all card errors wholesale.
   */
  beginSessionRecovery(): void {
    if (this.sessionExpired()) return;
    this.sessionExpired.set(true);
    this.stopPolling();
    this.stopListingConfirm();
    this.stopStream();
    this.walletSvc.dropSession();
    this.clearSessionErrors();
  }

  /**
   * Ask for a fresh wallet signature in place. With a connected address and
   * provider, run the existing SIWE flow; otherwise fall through to a clean
   * sign-in state (no wallet to re-sign with).
   */
  reauth(): void {
    if (this.walletSvc.address() !== null && this.walletSvc.provider !== null) {
      void this.walletSvc.startSession();
    } else {
      this.abandonRecovery();
    }
  }

  /**
   * Give up on recovery: clear the prompt, dismiss the wallet error, and run
   * the full disconnect discipline so no card, filter or timer survives.
   */
  abandonRecovery(): void {
    this.sessionExpired.set(false);
    this.walletSvc.dismissError();
    this.disconnect();
  }

  /** Drop the has-data marks so a fresh sign-in shows skeletons again, and
   *  reset the pagination cursors so a new session never merges the previous
   *  session's older pages into its own. */
  private clearReadState(): void {
    this.balanceHasData.set(false);
    this.listingsHasData.set(false);
    this.marketHasData.set(false);
    this.keysHasData.set(false);
    this.usageHasData.set(false);
    this.fundingHasData.set(false);
    this.chainsHasData.set(false);
    this.modelsHasData.set(false);
    this.attestationHasData.set(false);
    this.advertisedBaseUrl.set('');
    this.usagePage.set(1);
    this.usageTotal.set(0);
    this.listingsCursor.set(null);
  }

  private toApiKey(k: Key): ApiKey {
    return {
      id: k.id,
      display: keyDisplay(k.id),
      label: k.label,
      created: k.createdAt,
      lastUsedAt: k.lastUsedAt,
      requests: k.requestCount,
      revoked: k.revoked,
    };
  }

  private toUsageRow(r: UsageRowT): UsageRow {
    return {
      id: r.id,
      time: r.occurredAt,
      key: r.keyId,
      model: r.model,
      prompt: r.promptTokens,
      cached: r.cachedTokens,
      completion: r.completionTokens,
      credits: r.credits / MICRO_CREDITS_PER_CREDIT,
      result: r.status,
      latencyMs: r.latencyMs,
      error: r.error,
      // A commitment is only real when all three facts are present together;
      // anything partial reads as awaiting settlement.
      commitment:
        r.batchIndex !== null &&
        r.root !== null &&
        r.root !== '' &&
        r.commitTx !== null &&
        r.commitTx !== ''
          ? {
              batchIndex: r.batchIndex,
              root: r.root,
              commitTx: r.commitTx,
            }
          : null,
    };
  }

  // ---- metering stream -----------------------------------------------
  /** 'off' = no session expected (or demo mode); 'disconnected' = a
   *  session exists but the socket is not open (connecting or dropped);
   *  'connected' = socket open. The balance-fallback timer keys off this
   *  state, and it is the single source of truth for stream connectivity. */
  readonly streamState = signal<StreamState>('off');
  private es: EventSource | null = null;
  private reconnectTimer:
    | ReturnType<typeof setTimeout>
    | ReturnType<typeof setInterval>
    | undefined;
  private backoffMs = 1000;

  /**
   * Open the metering stream and wire it to the store. Idempotent, and a
   * no-op in demo mode, where no stream server exists. The socket's own
   * auto-retry is suppressed: onerror closes it and the backoff below
   * drives the reconnect instead.
   */
  startStream(): void {
    if (this.es !== null) return;
    if (environment.useMock) return;
    this.streamState.set('disconnected');
    // EventSource cannot send a bearer header, so the token rides as a query
    // parameter; the server folds it into the same bearer check.
    const token = this.walletSvc.token() ?? '';
    const es = new EventSource(
      `${this.baseUrl}/console/stream?token=${encodeURIComponent(token)}`,
    );
    this.es = es;
    es.onopen = () => {
      this.backoffMs = 1000;
      this.streamState.set('connected');
    };
    es.onerror = () => {
      es.close();
      this.es = null;
      this.streamState.set('disconnected');
      // The stream is down: catch the balance up now. The shared loop owns
      // the 15 s cadence whether the stream is up or down, so no separate
      // fallback timer is needed.
      void this.loadAccount();
      this.reconnectTimer = setTimeout(
        () => this.startStream(),
        this.backoffMs,
      );
      this.backoffMs = Math.min(this.backoffMs * 2, 30_000);
    };
    // The DOM lib types addEventListener callbacks as Event, so each
    // handler reads the payload through a MessageEvent cast.
    es.addEventListener('balance', (ev: Event) =>
      this.applyBalance((ev as MessageEvent).data),
    );
    es.addEventListener('usage', (ev: Event) =>
      this.applyUsage((ev as MessageEvent).data),
    );
  }

  /** Tear down the stream and its timers. Safe to call when idle. */
  stopStream(): void {
    if (this.reconnectTimer !== undefined) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = undefined;
    }
    if (this.es !== null) {
      this.es.close();
      this.es = null;
    }
    this.streamState.set('off');
  }

  /** Apply a `balance` event: the authoritative ledger figures that
   *  supersede a stale load error. Every rendered balance view is a
   *  computed over the `balance` and `locked` signals, so this one set
   *  updates all of them. */
  private applyBalance(data: string): void {
    try {
      // The wire event carries the five ledger figures in integer
      // micro-credits; `spendableMicro` is the balance and `lockedMicro`
      // the open-listing hold.
      const payload = JSON.parse(data) as {
        spendableMicro?: unknown;
        lockedMicro?: unknown;
      };
      if (typeof payload.spendableMicro === 'number') {
        this.balance.set(payload.spendableMicro / MICRO_CREDITS_PER_CREDIT);
        this.balanceError.set(null);
      }
      if (typeof payload.lockedMicro === 'number') {
        this.locked.set(payload.lockedMicro / MICRO_CREDITS_PER_CREDIT);
      }
    } catch {
      // Malformed event: keep the last known value.
    }
  }

  /** Apply a `usage` event: prepend the row, bounding the in-memory log.
   *  The balance is NOT debited here; it arrives via `balance` events. */
  private applyUsage(data: string): void {
    try {
      // A wire event is a `/console/usage` entry; the fake in the specs
      // sends the client row shape directly.
      const parsed = JSON.parse(data) as UsageWire | UsageRowT;
      const row = this.toUsageRow(
        'timestamp' in parsed ? fromUsageWire(parsed) : parsed,
      );
      this.usage.update((rows) => [row, ...rows].slice(0, USAGE_CAP));
    } catch {
      // Malformed event: keep the existing log.
    }
  }

  // ---- polling --------------------------------------------------------

  /**
   * Fire every live read once, in parallel. The shared loop calls this on
   * each tick; the values swap in place, so a failed read leaves its stale
   * figure visible while its own error signal surfaces.
   */
  private refreshAll(): void {
    void this.loadAccount();
    void this.loadUsage();
    void this.pollOnce();
    void this.loadListings();
    void this.loadMarket();
    void this.loadAttestation();
  }

  /**
   * Arm the console-wide loop for this session. The shared service owns the
   * 15 s cadence; here we only reset the live clock and do one immediate full
   * refresh so a fresh sign-in is not blank for a whole interval.
   */
  startPolling(): void {
    this.stopPolling();
    this.now.set(Date.now());
    this.polling.start();
    this.clockTimer = setInterval(() => this.now.set(Date.now()), 1_000);
    void this.refreshAll();
  }

  /** Stop the shared loop and the live clock. Safe to call when not polling. */
  stopPolling(): void {
    this.polling.stop();
    if (this.clockTimer !== undefined) {
      clearInterval(this.clockTimer);
      this.clockTimer = undefined;
    }
  }

  /**
   * One funding read. Merges the local-pending twin over the server's list,
   * publishes the history and the trace's current purchase, and surfaces any
   * failure (a 401 routes to session recovery). It never stops the loop: the
   * next tick fires regardless of what this one found or whether it failed.
   */
  private async pollOnce(): Promise<void> {
    try {
      const server = (await this.api.fundingList()).map((p) => {
        // A direct purchase carries the block time as its start; restore the
        // wallet-return time when this browser sent it, so elapsed is honest.
        if (!isDirectPurchase(p)) return p;
        const ls = this.localStarts.get(p.ccTx ?? '') ?? this.localStarts.get(p.purchaseId);
        return ls === undefined ? p : { ...p, startedAt: ls };
      });
      // The payment we just sent stays at the top until the backend lists
      // it under the same id (the source transaction hash).
      const local = this.localPending();
      let list = server;
      if (local !== null) {
        if (server.some((p) => p.purchaseId === local.purchaseId || p.ccTx === local.purchaseId)) {
          this.localPending.set(null);
        } else {
          list = [local, ...server];
        }
      }

      this.fundingHistory.set(list);
      this.fundingHasData.set(true);
      if (list.length === 0) {
        this.fundingStatus.set(null);
        return;
      }
      // The trace follows the user's pick when they made one, else the
      // newest purchase still in flight, else the newest one, settled.
      const inFlight = list.find((p) => p.state !== 'settled');
      const picked = this.selectedPurchaseId();
      const s = (picked !== null ? list.find((p) => p.purchaseId === picked) : undefined) ?? inFlight ?? list[0];
      this.fundingStatus.set(s);
      this.fundingError.set(null);
    } catch (e) {
      // Transient error: keep the last known status, surface the failure,
      // and retry on the next tick.
      this.handleLoadError((m) => this.fundingError.set(m), e);
    } finally {
      this.fundingLoading.set(false);
    }
  }

  /**
   * Register a payment this browser just sent. It shows as pending at the
   * top of the history and as the live trace, and polling runs until the
   * backend reports it. When `direct` is true the row settles in the very
   * transaction that pays it, so it carries the source block 0 marker and
   * the trace renders it as one step.
   */
  announcePurchase(
    txHash: string,
    amountUsd: number,
    token: string,
    chainKey = 0,
    chainName = '',
    direct = false,
  ): void {
    const now = new Date().toISOString();
    const entry: FundingStatus = {
      purchaseId: txHash,
      state: 'pending',
      ethTx: txHash,
      ethBlock: direct ? 0 : null,
      attestedHeight: null,
      chainTip: null,
      builderCeiling: null,
      ccTx: null,
      ccBlock: null,
      credits: null,
      amountUsd,
      token,
      startedAt: now,
      updatedAt: now,
      chainKey,
      chainName,
    };
    if (direct) this.localStarts.set(txHash, now);
    this.localPending.set(entry);
    this.selectedPurchaseId.set(null);
    void this.pollOnce();
  }

  /** Show the trace of one purchase from the history. */
  selectPurchase(purchaseId: string): void {
    const p = this.fundingHistory().find((x) => x.purchaseId === purchaseId);
    if (p === undefined) return;
    this.selectedPurchaseId.set(purchaseId);
    this.fundingStatus.set(p);
  }

  /**
   * Register a listing this browser just opened. The row comes back from
   * the create call already in wire shape, so it is announced as is: open
   * at the top of the listings until the backend's own read carries it,
   * with the lock figure moving through the same confirmation loop as any
   * other credit.
   */
  announceListing(row: Listing): void {
    const prevLocked = this.locked();
    this.localListing.set(row);
    void this.loadListings();
    this.confirmLockedChange(prevLocked);
  }

  /**
   * Publish the overview's buy card for a settled purchase. The settlement
   * call returns only after the server has verified the transfer and moved
   * the credits, so the card is complete from the first frame; the balance
   * poll behind it re-reads the account like any other credit movement.
   */
  announceBuy(txHash: string, credits: number, priceText: string): void {
    this.buySnap.set({
      txHash,
      credits,
      priceText,
      startedAt: new Date().toISOString(),
    });
    void this.loadAccount();
    void this.loadListings();
    void this.loadMarket();
  }

  /**
   * Cancel an open listing. The row's state follows the server, never the
   * click: on success the list refreshes from the API, on failure the
   * row is left open and `listingsError` is set.
   */
  async cancelListing(id: string): Promise<void> {
    // A new action clears the card before the attempt: a stale rejection
    // notice never outlives the next click that started it.
    this.listingsError.set(null);
    const prevLocked = this.locked();
    try {
      await this.sell.cancel(id);
      await this.loadListings();
      this.confirmLockedChange(prevLocked);
    } catch (e) {
      this.handleLoadError((m) => this.listingsError.set(m), e);
      throw e;
    }
  }

  // ---- actions --------------------------------------------------------

  connect(wallet: Wallet): void {
    this.wallet.set(wallet);
    // Turn the connected address into a session (nonce → sign → verify).
    // The effect above loads balance, keys and usage once it exists.
    void this.walletSvc.startSession();
  }

  disconnect(): void {
    this.selectedPurchaseId.set(null);
    this.localPending.set(null);
    this.localListing.set(null);
    this.buySnap.set(null);
    this.transactionFilter.set('all');
    this.usageStatusFilter.set('all');
    this.usageKey.set('');
    this.usageModel.set('');
    this.usageFrom.set(0);
    this.usageTo.set(0);
    this.usagePage.set(1);
    this.wallet.set(null);
    this.walletSvc.clearSession();
    this.stopPolling();
    this.stopListingConfirm();
    this.stopStream();
    // No error state may survive a session change: wallet 2 must never see
    // wallet 1's cards. Reset every session-scoped error wholesale.
    this.clearSessionErrors();
    // Drop the loaded history and its pagination state so a fresh sign-in
    // starts clean rather than merging the previous session's older pages.
    this.usage.set([]);
    this.usageTotal.set(0);
    this.usageFacets.set({ prefixes: [], models: [] });
    this.proofChecks.set({});
    this.fundingHistory.set([]);
    this.listings.set([]);
    this.listingsCursor.set(null);
  }

  /** Drop every error card the store publishes. */
  clearSessionErrors(): void {
    this.balanceError.set(null);
    this.chainsError.set(null);
    this.modelsError.set(null);
    this.keysError.set(null);
    this.usageError.set(null);
    this.fundingError.set(null);
    this.listingsError.set(null);
    this.marketError.set(null);
    this.attestationError.set(null);
    this.sell.dismissError();
    this.buy.dismissError();
    this.deposit.dismissError();
  }

  /** Dismiss the balance card (its X control). */
  clearBalanceError(): void {
    this.balanceError.set(null);
  }

  /** Dismiss the payment-chains card (its X control). */
  clearChainsError(): void {
    this.chainsError.set(null);
  }

  /** Dismiss the model-catalog card (its X control). */
  clearModelsError(): void {
    this.modelsError.set(null);
  }

  /** Dismiss the keys card (its X control). */
  clearKeysError(): void {
    this.keysError.set(null);
  }

  /** Dismiss the usage card (its X control). */
  clearUsageError(): void {
    this.usageError.set(null);
  }

  /** Dismiss the funding-poll card (its X control). */
  clearFundingError(): void {
    this.fundingError.set(null);
  }

  /** Dismiss the listings card (its X control). */
  clearListingsError(): void {
    this.listingsError.set(null);
  }

  /** Dismiss the market card (its X control). */
  clearMarketError(): void {
    this.marketError.set(null);
  }

  /** Dismiss the attestation card (its X control). */
  clearAttestationError(): void {
    this.attestationError.set(null);
  }

  setAmount(value: number): void {
    this.amount.set(Number.isFinite(value) ? value : 0);
  }

  /** Mint a key server-side, then refresh the list and the account.
   *  Returns the server's answer so the caller can show the plaintext once.
   *  On failure it publishes `keysError` and rethrows so the caller can
   *  surface it. */
  async createKey(label: string): Promise<CreatedKey> {
    try {
      const created = await this.api.createKey(label);
      await this.loadKeys();
      await this.loadAccount();
      return created;
    } catch (e) {
      this.handleLoadError((m) => this.keysError.set(m), e);
      throw e;
    }
  }

  /** Revoke a key server-side, then refresh the list and the account.
   *  On failure the row is left active, `keysError` is set, and the error is
   *  rethrown so the caller can tell the user. */
  async revokeKey(id: string): Promise<void> {
    try {
      await this.api.revokeKey(id);
      await this.loadKeys();
      await this.loadAccount();
    } catch (e) {
      this.handleLoadError((m) => this.keysError.set(m), e);
      throw e;
    }
  }

  /** Charge one request against the balance. Unused by the mock's fixtures
   *  but this is the seam the metering stream will drive. */
  settle(row: UsageRow): void {
    this.usage.update((rows) => [row, ...rows]);
    this.balance.update((b) => Math.max(0, b - costOf({
      prompt: row.prompt ?? 0,
      cached: row.cached ?? 0,
      completion: row.completion ?? 0,
    })));
  }
}
