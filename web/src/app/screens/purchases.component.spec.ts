import { computed, signal } from '@angular/core';
import { Component } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter, Router } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { SellService } from '../core/sell';
import { BuyService } from '../core/buy';
import { ToastService } from '../core/toast';
import {
  Account, ApiError, AttestChain, ChainInfo,
  CreatedKey, DepositTarget, FundingStatus, Key,
  ListingPage, MarketConfirmBody, MarketPrepareBody, ModelList,
  Nonce, PreparedMarketTx, SLOTFARE_API, SlotfareApi,
  UsagePage, UsageProof, UsageQuery, VerifyResult,
} from '../core/api';
import { Listing, MarketListing, Model, Wallet } from '../core/models';
import { environment } from '../../environments/environment';
import { ConsoleSession, WalletService } from '../core/wallet';
import { OPENING_BALANCE, OPENING_LOCKED } from '../core/mock-data';
import { PurchasesComponent } from './purchases.component';

const tick = (): Promise<void> => new Promise((r) => setTimeout(r));

function makeFakeWallet(): WalletService {
  const address = signal<string | null>(null);
  const chainId = signal<string | null>(null);
  const connecting = signal(false);
  const error = signal<string | null>(null);
  const accountId = signal<string | null>(null);
  const token = signal<string | null>(null);
  const session = computed<ConsoleSession | null>(() => {
    const t = token();
    if (t === null) return null;
    const a = address();
    const id = accountId();
    if (a === null || id === null) return null;
    return { address: a, accountId: id, token: t };
  });
  return {
    providers: signal<Wallet[]>([]),
    address,
    chainId,
    connecting,
    error,
    accountId,
    token,
    session,
    provider: null,
    connect: () => undefined,
    startSession: () => undefined,
    clearSession: () => {
      token.set(null);
      accountId.set(null);
      error.set(null);
    },
  } as unknown as WalletService;
}

function makeFunding(overrides: Partial<FundingStatus> = {}): FundingStatus {
  return {
    purchaseId: 'p1',
    state: 'pending',
    ethTx: '0x9f2ad4e1c88b7605a3f0d2914e77bb60c4a1de0938f5c2b7a04e61d3be04c115',
    ethBlock: null,
    attestedHeight: null,
    chainTip: null,
    builderCeiling: null,
    ccTx: null,
    ccBlock: null,
    credits: null,
    amountUsd: 250,
    token: 'mUSDC',
    startedAt: '2026-08-22T20:00:00Z',
    updatedAt: '2026-08-22T20:00:00Z',
    chainKey: 1,
    chainName: 'Ethereum',
    ...overrides,
  };
}

class FakeApi implements SlotfareApi {
  fundingListData: FundingStatus[] = [];
  listingsData: Listing[] = [];
  /** 0 serves the whole set in one batch; otherwise it bounds the first
   *  batch and every load-more page, so the cursor goes live in the tests.
   *  The funding read is not paged (the server serves every deposit on
   *  each poll), so only listings pages. */
  listingsPageSize = 0;
  async nonce(_a: string): Promise<Nonce> {
    return { nonce: 'n', statement: 's', issuedAt: '', expiresAt: '' };
  }
  async verify(_a: string, _n: string, _s: string): Promise<VerifyResult> {
    return { accountId: '0xmockacct', token: 't', expiresAt: '' };
  }
  async account(): Promise<Account> {
    return {
      accountId: '0xmockacct',
      address: '0xabc',
      balanceCredits: OPENING_BALANCE,
      lockedCredits: OPENING_LOCKED,
      totalSpentCredits: 0,
      createdAt: '2026-06-14',
      baseUrl: '',
    };
  }
  async keys(): Promise<Key[]> {
    return [];
  }
  async createKey(_l: string): Promise<CreatedKey> {
    throw new ApiError(500, 'internal', 'no');
  }
  async revokeKey(_id: string): Promise<void> {}
  async usage(_q?: UsageQuery): Promise<UsagePage> {
    return { rows: [], nextCursor: null };
  }
  async funding(_id: string): Promise<FundingStatus> {
    throw new ApiError(500, 'internal', 'no');
  }
  async fundingList(): Promise<FundingStatus[]> {
    return this.fundingListData.map((s) => ({ ...s }));
  }
  async listings(before?: string): Promise<ListingPage> {
    let rows = this.listingsData.map((l) => ({ ...l }));
    const b = before === undefined ? undefined : Number(before);
    if (b !== undefined) {
      const idx = rows.findIndex((r) => Number(r.id) === b);
      rows = idx === -1 ? [] : rows.slice(idx + 1);
    }
    if (this.listingsPageSize > 0) rows = rows.slice(0, this.listingsPageSize);
    const last = rows[rows.length - 1];
    const tail = this.listingsData[this.listingsData.length - 1];
    const nextCursor =
      last !== undefined && tail !== undefined && Number.isFinite(Number(last.id)) && Number(last.id) !== Number(tail.id)
        ? String(last.id)
        : null;
    return { rows, nextCursor };
  }
  async market(): Promise<MarketListing[]> {
    return [];
  }
  async depositTarget(): Promise<DepositTarget> {
    throw new Error('not implemented');
  }
  async createListing(_body: { credits: number; price: number }): Promise<Listing> {
    throw new Error('not implemented');
  }
  async cancelListing(_id: string): Promise<Listing> {
    throw new Error('not implemented');
  }
  async payListing(_id: string, _signature: string): Promise<Listing> {
    throw new Error('not implemented');
  }
  async prepareMarket(_body: MarketPrepareBody): Promise<PreparedMarketTx> {
    throw new Error('not implemented');
  }
  async confirmMarket(_body: MarketConfirmBody): Promise<Listing> {
    throw new Error('not implemented');
  }

  async chains(): Promise<ChainInfo[]> {
    return [];
  }

  async attestation(): Promise<AttestChain[]> {
    return [];
  }
  async models(): Promise<ModelList> {
    return { models: [], baseUrl: '' };
  }

  /** EP-027-T05: the inclusion proof read; this screen does not exercise it. */
  async usageProof(_rowId: string): Promise<UsageProof> {
    throw new ApiError(500, 'internal', 'not implemented');
  }
}

@Component({ selector: 'slf-dummy', template: '' })
class DummyComponent {}

describe('PurchasesComponent', () => {
  // The store's stream gate is environment.useMock; keep it true so
  // these specs never open a real EventSource (jsdom has none).
  beforeAll(() => {
    environment.useMock = true;
  });
  let fixture: ComponentFixture<PurchasesComponent>;
  let fakeApi: FakeApi;
  let fakeWallet: WalletService;
  let store: ConsoleStore;

  function setup() {
    fakeApi = new FakeApi();
    fakeWallet = makeFakeWallet();
    TestBed.configureTestingModule({
      imports: [PurchasesComponent],
      providers: [
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: fakeWallet },
        { provide: SellService, useValue: {
          step: signal('idle'),
          error: signal<string | null>(null),
          txHash: signal<string | null>(null),
          busy: computed(() => false),
          list: async (_c: number, _s: string, _p: number): Promise<string> => '0xlisttx',
          cancel: async (_id: string): Promise<string> => '0xcanceltx',
          dismissError: () => undefined,
        } },
        { provide: BuyService, useValue: {
          step: signal('idle'),
          error: signal<string | null>(null),
          txHash: signal<string | null>(null),
          listingGone: signal(false),
          busy: computed(() => false),
          buy: async (_id: string, _p: number, _s: string): Promise<string> => '0xbuytx',
          dismissError: () => undefined,
        } },
        ConsoleStore,
        ToastService,
        provideRouter([
          { path: 'funding/:id', component: DummyComponent },
          { path: 'sell', component: DummyComponent },
          { path: 'trade/:id', component: DummyComponent },
        ]),
      ],
    });
    fixture = TestBed.createComponent(PurchasesComponent);
    store = TestBed.inject(ConsoleStore);
    fixture.detectChanges();
    // Establish a session so the store loads the account.
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
  }

  afterEach(() => {
    store.stopPolling();
  });

  function root(): HTMLElement {
    return fixture.nativeElement as unknown as HTMLElement;
  }

  function rows(): NodeListOf<HTMLTableRowElement> {
    return root().querySelectorAll('tbody tr');
  }

  function filterButton(label: string): HTMLButtonElement {
    const buttons = Array.from(root().querySelectorAll('.txn-filter'));
    const b = buttons.find((x) => x.textContent?.trim() === label);
    expect(b).toBeDefined();
    return b as HTMLButtonElement;
  }

  it('renders one merged table and All shows every row', async () => {
    setup();
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'settled', credits: 250_000, startedAt: '2026-08-24T10:00:00Z' }),
    ];
    fakeApi.listingsData = [
      { id: 'b1', listedTx: '0xbuytx1', state: 'sold', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-23T00:00:00Z' },
      { id: 's1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T00:00:00Z' },
    ];
    await store.loadListings();
    store.startPolling();
    await tick();
    fixture.detectChanges();

    expect(rows().length).toBe(3);
    expect(root().textContent).toContain('Transactions');
  });

  it('scrolls the table in its own box, never the page', async () => {
    setup();
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'settled', credits: 250_000, startedAt: '2026-08-24T10:00:00Z' }),
    ];
    fakeApi.listingsData = [
      { id: 's1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T00:00:00Z' },
    ];
    await store.loadListings();
    store.startPolling();
    await tick();
    fixture.detectChanges();

    const page = root().querySelector('section.page')!;
    expect(page).toBeTruthy();
    expect(page.classList).not.toContain('table-wrap');
    const table = page.querySelector('table.tbl')!;
    expect(table.closest('.table-wrap')).toBeTruthy();
    expect(table.parentElement!.classList).toContain('table-wrap');
  });

  it('clicking Buying hides sells and Selling hides deposits + buys; counts follow', async () => {
    setup();
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'settled', credits: 250_000, startedAt: '2026-08-24T10:00:00Z' }),
    ];
    fakeApi.listingsData = [
      { id: 'b1', listedTx: '0xbuytx1', state: 'sold', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-23T00:00:00Z' },
      { id: 's1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T00:00:00Z' },
    ];
    await store.loadListings();
    store.startPolling();
    await tick();
    fixture.detectChanges();

    filterButton('Buying').click();
    fixture.detectChanges();
    expect(store.transactionCount()).toBe(2);
    expect(rows().length).toBe(2);

    filterButton('Selling').click();
    fixture.detectChanges();
    expect(store.transactionCount()).toBe(1);
    expect(rows().length).toBe(1);

    filterButton('All').click();
    fixture.detectChanges();
    expect(store.transactionCount()).toBe(3);
    expect(rows().length).toBe(3);
  });

  it('links a deposit to its trace, an open sell to the sell screen, and finished trades to the detail view', async () => {
    setup();
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'settled', credits: 250_000, startedAt: '2026-08-24T10:00:00Z' }),
    ];
    fakeApi.listingsData = [
      { id: 'b1', listedTx: '0xbuytx1', state: 'sold', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-23T00:00:00Z' },
      { id: 's1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T00:00:00Z' },
      { id: 'c1', listedTx: '0xlist3', state: 'cancelled', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-21T00:00:00Z' },
    ];
    await store.loadListings();
    store.startPolling();
    await tick();
    fixture.detectChanges();

    const all = root().querySelectorAll('tbody tr');
    const depRow = Array.from(all).find((tr) => tr.textContent?.includes('Deposit'));
    const openSellRow = Array.from(all).find((tr) => tr.textContent?.includes('Sell') && tr.textContent?.includes('For sale'));
    const boughtRow = Array.from(all).find((tr) => tr.textContent?.includes('Buy') && tr.textContent?.includes('Bought'));
    const cancelledRow = Array.from(all).find((tr) => tr.textContent?.includes('Sell') && tr.textContent?.includes('Cancelled'));
    expect(depRow).toBeDefined();
    expect(openSellRow).toBeDefined();
    expect(boughtRow).toBeDefined();
    expect(cancelledRow).toBeDefined();

    // Deposit row is a router link (role="link"), not an external link.
    expect(depRow!.getAttribute('role')).toBe('link');
    expect(depRow!.querySelector('a[href]')).toBeNull();

    // An open sell row stays a router link to the Sell screen.
    expect(openSellRow!.getAttribute('role')).toBe('link');
    expect(openSellRow!.querySelector('a[href]')).toBeNull();

    // A bought row now opens the trade detail, not an external explorer link.
    expect(boughtRow!.getAttribute('role')).toBe('link');
    expect(boughtRow!.querySelector('a[href]')).toBeNull();

    // A cancelled sell row opens the trade detail too.
    expect(cancelledRow!.getAttribute('role')).toBe('link');
    expect(cancelledRow!.querySelector('a[href]')).toBeNull();
  });

  it('shows the network in its own column: wire chain for deposits, Solana for market rows', async () => {
    setup();
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'settled', credits: 250_000, chainName: 'Ethereum', startedAt: '2026-08-24T10:00:00Z' }),
    ];
    fakeApi.listingsData = [
      { id: 's1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: false, createdAt: '2026-08-22T00:00:00Z' },
    ];
    await store.loadListings();
    store.startPolling();
    await tick();
    fixture.detectChanges();

    const all = root().querySelectorAll('tbody tr');
    const depRow = Array.from(all).find((tr) => tr.textContent?.includes('Deposit'));
    const sellRow = Array.from(all).find((tr) => tr.textContent?.includes('Sell'));
    expect(depRow).toBeDefined();
    expect(sellRow).toBeDefined();

    // The deposit row's Network cell carries its source chain as a badge.
    const depNetwork = depRow!.querySelector('td[data-th="Network"] .chain-badge');
    expect(depNetwork).not.toBeNull();
    expect(depNetwork!.textContent?.trim()).toBe('Ethereum');
    // The sell row settles on Solana, so its Network cell says so.
    const sellNetwork = sellRow!.querySelector('td[data-th="Network"] .chain-badge');
    expect(sellNetwork).not.toBeNull();
    expect(sellNetwork!.textContent?.trim()).toBe('Solana');

    // The deposit row's Date cell renders through the shared local-time
    // helper: the wire's UTC instant in the runner's local zone, no suffix.
    const depDate = depRow!.querySelectorAll('td')[0];
    expect(depDate.textContent?.trim()).toBe(new Date('2026-08-24T10:00:00Z').toLocaleString());
  });

  it('renders type, status and network in their own columns', async () => {
    setup();
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'pending', credits: 250_000, chainName: 'Ethereum', startedAt: '2026-08-24T10:00:00Z' }),
    ];
    fakeApi.listingsData = [
      { id: 's1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-23T00:00:00Z' },
      { id: 's2', listedTx: '0xlist2', state: 'sold', role: 'selling', credits: 4_000, price: 400, token: 'mUSD1', matured: true, createdAt: '2026-08-21T00:00:00Z' },
      { id: 'b1', listedTx: '0xbuytx1', state: 'sold', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-22T00:00:00Z' },
    ];
    await store.loadListings();
    store.startPolling();
    await tick();
    fixture.detectChanges();

    const cell = (tr: Element, which: string): string | null => {
      const td = tr.querySelector(`td[data-th="${which}"]`);
      return td ? td.textContent?.trim() : null;
    };

    const depRow = Array.from(rows()).find((tr) => cell(tr, 'Type') === 'Deposit');
    expect(depRow).toBeDefined();
    expect(cell(depRow!, 'Status')).toBe('Pending');
    expect(cell(depRow!, 'Network')).toBe('Ethereum');

    // A sold sell row reads Type Sell / Status Sold from the seller's side.
    const soldSellRow = Array.from(rows()).find((tr) => cell(tr, 'Type') === 'Sell' && cell(tr, 'Status') === 'Sold');
    expect(soldSellRow).toBeDefined();
    expect(cell(soldSellRow!, 'Network')).toBe('Solana');

    const sellRow = Array.from(rows()).find((tr) => cell(tr, 'Type') === 'Sell');
    expect(sellRow).toBeDefined();
    expect(cell(sellRow!, 'Status')).toBe('Matured');
    expect(cell(sellRow!, 'Network')).toBe('Solana');

    const buyRow = Array.from(rows()).find((tr) => cell(tr, 'Type') === 'Buy');
    expect(buyRow).toBeDefined();
    expect(cell(buyRow!, 'Status')).toBe('Bought');
    expect(cell(buyRow!, 'Network')).toBe('Solana');
  });

  it('filter partitions agree with the Type column', async () => {
    setup();
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'pending', credits: 250_000, chainName: 'Ethereum', startedAt: '2026-08-24T10:00:00Z' }),
    ];
    fakeApi.listingsData = [
      { id: 's1', listedTx: '0xlist1', state: 'open', role: 'selling', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-23T00:00:00Z' },
      { id: 'b1', listedTx: '0xbuytx1', state: 'sold', role: 'buying', credits: 5_000, price: 500, token: 'mUSD1', matured: true, createdAt: '2026-08-22T00:00:00Z' },
    ];
    await store.loadListings();
    store.startPolling();
    await tick();
    fixture.detectChanges();

    const types = (): string[] =>
      Array.from(rows()).map((tr) => tr.querySelector('td[data-th="Type"]')?.textContent?.trim() ?? '');

    // All: every kind shows.
    expect(types().sort()).toEqual(['Buy', 'Deposit', 'Sell']);

    filterButton('Buying').click();
    fixture.detectChanges();
    expect(types().sort()).toEqual(['Buy', 'Deposit']);

    filterButton('Selling').click();
    fixture.detectChanges();
    expect(types()).toEqual(['Sell']);

    filterButton('All').click();
    fixture.detectChanges();
    expect(types().sort()).toEqual(['Buy', 'Deposit', 'Sell']);
  });

  function listing(
    id: string,
    role: 'selling' | 'buying',
    createdAt: string,
  ): Listing {
    return {
      id,
      listedTx: `0x${id}`,
      state: 'sold',
      role,
      credits: 5_000,
      price: 500,
      token: 'mUSD1',
      matured: true,
      createdAt,
    };
  }

  it('load more appends older transactions without duplicates and hides at exhaustion', async () => {
    setup();
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'settled', credits: 250_000, startedAt: '2026-08-24T10:00:00Z' }),
    ];
    fakeApi.listingsPageSize = 1;
    fakeApi.listingsData = [
      listing('1', 'selling', '2026-08-23T00:00:00Z'),
      listing('0', 'buying', '2026-08-22T00:00:00Z'),
    ];
    await store.loadListings();
    store.startPolling();
    await tick();
    fixture.detectChanges();

    // One market row in the first batch; the cursor is live, so the control
    // shows.
    expect(store.listings().map((l) => l.id)).toEqual(['1']);
    expect(store.transactions().length).toBe(2);
    expect(store.transactionsHasMore()).toBe(true);
    const more = root().querySelector('.load-more .btn') as HTMLButtonElement;
    expect(more).toBeTruthy();
    expect(more.textContent?.trim()).toBe('Load more');

    more.click();
    fixture.detectChanges();
    await tick();

    // The older row lands with no duplicate across the boundary, and the
    // control disappears once the history is exhausted.
    expect(store.listings().map((l) => l.id)).toEqual(['1', '0']);
    expect(store.transactions().length).toBe(3);
    const ids = store.transactions().map((r) => r.id);
    expect(new Set(ids).size).toBe(ids.length);
    expect(store.transactionsHasMore()).toBe(false);
    fixture.detectChanges();
    expect(root().querySelector('.load-more .btn')).toBeNull();
  });

  it('the active filter applies across every loaded page', async () => {
    setup();
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'settled', credits: 250_000, startedAt: '2026-08-24T10:00:00Z' }),
    ];
    fakeApi.listingsPageSize = 1;
    fakeApi.listingsData = [
      listing('1', 'buying', '2026-08-23T00:00:00Z'),
      listing('0', 'selling', '2026-08-22T00:00:00Z'),
    ];
    await store.loadListings();
    store.startPolling();
    await tick();
    // The selling row is on the second page; load it before filtering.
    await store.loadMoreTransactions();
    await tick();
    fixture.detectChanges();

    expect(store.transactions().length).toBe(3);
    // Selling: only the sell row. Buying: the deposit plus the loaded buy row.
    filterButton('Selling').click();
    fixture.detectChanges();
    expect(Array.from(rows()).length).toBe(1);
    filterButton('Buying').click();
    fixture.detectChanges();
    expect(Array.from(rows()).length).toBe(2);
  });

  it('a poll refresh keeps the already-loaded older pages', async () => {
    setup();
    fakeApi.fundingListData = [
      makeFunding({ purchaseId: 'dep1', state: 'settled', credits: 250_000, startedAt: '2026-08-24T10:00:00Z' }),
      makeFunding({ purchaseId: 'dep2', state: 'settled', credits: 100_000, startedAt: '2026-08-20T10:00:00Z' }),
    ];
    fakeApi.listingsPageSize = 1;
    fakeApi.listingsData = [
      listing('1', 'selling', '2026-08-23T00:00:00Z'),
      listing('0', 'buying', '2026-08-22T00:00:00Z'),
    ];
    await store.loadListings();
    store.startPolling();
    await tick();

    // The first poll carries every deposit (the funding read is not paged);
    // only the market rows page.
    expect(store.fundingHistory().map((p) => p.purchaseId)).toEqual(['dep1', 'dep2']);
    expect(store.listings().map((l) => l.id)).toEqual(['1']);
    expect(store.transactionsHasMore()).toBe(true);

    await store.loadMoreTransactions();
    await tick();
    expect(store.listings().map((l) => l.id)).toEqual(['1', '0']);

    // A fresh poll plus a listings refresh must merge the newest batch over
    // the preserved older page, never discarding it.
    store.startPolling();
    await tick();
    await store.loadListings();
    expect(store.fundingHistory().map((p) => p.purchaseId)).toEqual(['dep1', 'dep2']);
    expect(store.listings().map((l) => l.id)).toEqual(['1', '0']);
  });
});
