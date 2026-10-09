import { computed, signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ConsoleStore } from '../core/console-store';
import { ToastService } from '../core/toast';
import {
  Account, ApiError, AttestChain, ChainInfo,
  CreatedKey, DepositTarget, FundingStatus, Key,
  ListingPage, MarketConfirmBody, MarketPrepareBody, MockSlotfareApi,
  ModelList, Nonce, PreparedMarketTx, SLOTFARE_API,
  SlotfareApi, UsagePage, UsageProof, UsageQuery,
  UsageRowT, VerifyResult,
} from '../core/api';
import { ConsoleSession, WalletService } from '../core/wallet';
import { Listing, MarketListing, Model, Wallet } from '../core/models';
import { environment } from '../../environments/environment';
import { KEYS, DAILY_SPEND, DAILY_REQUESTS } from '../core/mock-data';
import { DAY_MS } from '../core/burn';
import { short } from '../core/trace';
import { UsageComponent } from './usage.component';

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

class FakeApi implements SlotfareApi {
  lastUsageQuery: UsageQuery | null = null;
  usageCalls = 0;
  /** Mutable rows returned by usage(). */
  usageRows: UsageRowT[] = [];
  /** When true, the next served proof's sibling path is corrupted once:
   *  the walk lands on the wrong root, so the check fails. */
  tamperProofNext = false;
  /** The mock serves the fixture's commitment tree; when a test feeds the
   *  fixture's own rows, the ids line up with its proofs. */
  private readonly proofSource = new MockSlotfareApi();
  /**
   * How many rows every batch returns; 0 means the server serves the whole
   * set in one batch. Bounding the batches is what makes the cursor go live
   * in the load-more tests while leaving the existing specs (which set 0)
   * unchanged.
   */
  usagePageSize = 0;

  async nonce(_address: string): Promise<Nonce> {
    return { nonce: 'n', statement: 's', issuedAt: '', expiresAt: '' };
  }
  async verify(
    _a: string,
    _n: string,
    _s: string,
  ): Promise<VerifyResult> {
    return { accountId: '0xmockacct', token: 't', expiresAt: '' };
  }
  async account(): Promise<Account> {
    return {
      accountId: '0xmockacct',
      address: '0xabc',
      balanceCredits: 100,
      lockedCredits: 0,
      totalSpentCredits: 0,
      createdAt: '2026-06-14',
      baseUrl: '',
    };
  }
  async keys(): Promise<Key[]> {
    return KEYS.map((k) => ({
      id: k.id,
      label: k.label,
      createdAt: k.created,
      lastUsedAt: k.lastUsedAt,
      requestCount: k.requests,
      revoked: k.revoked,
    }));
  }
  async createKey(_label: string): Promise<CreatedKey> {
    throw new ApiError(500, 'internal', 'no');
  }
  async revokeKey(_id: string): Promise<void> {}
  async usage(query?: UsageQuery): Promise<UsagePage> {
    this.usageCalls += 1;
    this.lastUsageQuery = query ?? null;
    let rows = this.usageRows.map((r) => ({ ...r }));
    const facets = {
      prefixes: [...new Set(rows.map((r) => r.keyId))],
      models: [...new Set(rows.map((r) => r.model))],
    };
    if (query?.key && query.key !== 'all') {
      rows = rows.filter((r) => r.keyId === query.key);
    }
    if (query?.model) {
      rows = rows.filter((r) => r.model === query.model);
    }
    if (query?.errors) {
      rows = rows.filter((r) => r.error !== null);
    }
    const total = rows.length;
    // The numbered read (?page): one page of the filtered set, newest first,
    // with the matching total and facets; no cursor rides it.
    if (query?.page !== undefined) {
      const cap = query.limit ?? 25;
      rows = rows.slice((query.page - 1) * cap, query.page * cap);
      return { rows, nextCursor: null, total, facets };
    }
    // The unnumbered form keeps the before-id cursor for the chart's range
    // stand-in and any load-more-style reads.
    if (query?.before !== undefined) {
      const idx = rows.findIndex((r) => Number(r.id.slice(1)) === query.before);
      rows = idx === -1 ? [] : rows.slice(idx + 1);
    }
    if (this.usagePageSize > 0) rows = rows.slice(0, this.usagePageSize);
    else if (query?.limit !== undefined) rows = rows.slice(0, query.limit);
    const last = rows[rows.length - 1];
    const tail = this.usageRows[this.usageRows.length - 1];
    const nextCursor =
      last !== undefined && tail !== undefined && last.id !== tail.id
        ? Number(last.id.slice(1))
        : null;
    return { rows, nextCursor };
  }
  async funding(_purchaseId: string): Promise<FundingStatus> {
    throw new ApiError(500, 'internal', 'no');
  }
  async fundingList(): Promise<FundingStatus[]> {
    return [];
  }
  async listings(_before?: string): Promise<ListingPage> {
    return { rows: [], nextCursor: null };
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
  async usageProof(rowId: string): Promise<UsageProof> {
    const proof = await this.proofSource.usageProof(rowId);
    if (this.tamperProofNext) {
      this.tamperProofNext = false;
      const p = proof.path;
      return { ...proof, path: p.length > 1 ? [p[p.length - 1], ...p.slice(0, -1)] : p };
    }
    return proof;
  }
}

describe('UsageComponent', () => {
  // The store's stream gate is environment.useMock; keep it true so
  // these specs never open a real EventSource (jsdom has none).
  beforeAll(() => {
    environment.useMock = true;
  });
  let fixture: ComponentFixture<UsageComponent>;
  let fakeApi: FakeApi;
  let fakeWallet: WalletService;

  function setup() {
    fakeApi = new FakeApi();
    fakeWallet = makeFakeWallet();
    TestBed.configureTestingModule({
      imports: [UsageComponent],
      providers: [
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: fakeWallet },
        ConsoleStore,
        ToastService,
      ],
    });
    fixture = TestBed.createComponent(UsageComponent);
    fixture.detectChanges();
    // Establish a session so the store loads the key list.
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
  }

  afterEach(() => TestBed.inject(ConsoleStore).stopPolling());

  function segButtons(groupLabel: string): HTMLButtonElement[] {
    const group = fixture.nativeElement.querySelector(
      `.seg[aria-label="${groupLabel}"]`,
    );
    return Array.from(group.querySelectorAll('button'));
  }

  function filterButtons(): HTMLButtonElement[] {
    return Array.from(
      fixture.nativeElement.querySelectorAll('.txn-filters .txn-filter'),
    ) as HTMLButtonElement[];
  }

  it('renders All, Metered, Error in that order, with All active by default', () => {
    setup();
    const buttons = filterButtons();
    expect(buttons.map((b) => b.textContent?.trim())).toEqual([
      'All',
      'Metered',
      'Error',
    ]);
    // No key label leaks into the status-chip group.
    const groupText = (fixture.nativeElement.querySelector(
      '.txn-filters-log',
    ) as Element).textContent ?? '';
    for (const label of ['prod-router', 'batch-eval', 'staging', 'laptop-cli']) {
      expect(groupText).not.toContain(label);
    }
    expect(buttons[0].getAttribute('aria-current')).toBe('true');
    expect(buttons[1].getAttribute('aria-current')).toBeNull();
  });

  it('clicking Metered shows exactly the settled rows and issues no request', async () => {
    setup();
    fakeApi.usageRows = [
      { id: 'u0', occurredAt: '14:22:07', keyId: '7Qk4', model: 'llama-3.2-3b', promptTokens: 8_412, cachedTokens: 6_144, completionTokens: 1_206, credits: 126_444, status: 'ok', latencyMs: 412, error: null, batchIndex: null, root: null, commitTx: null },
      { id: 'u1', occurredAt: '14:21:54', keyId: '7Qk4', model: 'qwen3-8-27b', promptTokens: 2_190, cachedTokens: 0, completionTokens: 884, credits: 48_420, status: 'ok', latencyMs: 391, error: null, batchIndex: null, root: null, commitTx: null },
      { id: 'u2', occurredAt: '14:20:12', keyId: '2Mv9', model: 'llama-3.2-3b', promptTokens: null, cachedTokens: null, completionTokens: null, credits: 0, status: 'error', latencyMs: null, error: '400 context_length', batchIndex: null, root: null, commitTx: null },
    ];
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    fixture.detectChanges();
    const callsBefore = fakeApi.usageCalls;

    // All shows every row, error row included.
    const allRows = Array.from(
      fixture.nativeElement.querySelectorAll('tbody tr'),
    ) as Element[];
    expect(allRows.length).toBe(3);
    expect(allRows[2].textContent).toContain('400 context_length');

    filterButtons()[1].click();
    fixture.detectChanges();
    await tick();

    expect(fakeApi.usageCalls).toBe(callsBefore);
    const rows = Array.from(
      fixture.nativeElement.querySelectorAll('tbody tr'),
    ) as Element[];
    expect(rows.length).toBe(2);
    // The error row is not metered: its pill is gone from the table.
    expect(rows.map((r) => r.textContent).join('')).not.toContain(
      '400 context_length',
    );
    // The tfoot counts the filtered rows, not the full log.
    expect(fixture.nativeElement.outerHTML).toContain('Totals \u00b7 2 requests shown');

    // Error rides the server query, so it issues a request and shows only
    // the failed rows the wire returns.
    filterButtons()[2].click();
    fixture.detectChanges();
    await tick();
    fixture.detectChanges();

    expect(fakeApi.usageCalls).toBeGreaterThan(callsBefore);
    expect(fakeApi.lastUsageQuery?.errors).toBe(true);
    const errorRows = Array.from(
      fixture.nativeElement.querySelectorAll('tbody tr'),
    ) as Element[];
    expect(errorRows.length).toBe(1);
    expect(errorRows[0].textContent).toContain('400 context_length');
    expect(fixture.nativeElement.outerHTML).toContain('Totals \u00b7 1 requests shown');
  });

  it('duration renders humanized; a missing duration renders a dash', async () => {
    setup();
    fakeApi.usageRows = [
      { id: 'u0', occurredAt: '14:22:07', keyId: '7Qk4', model: 'llama-3.2-3b', promptTokens: 100, cachedTokens: 0, completionTokens: 50, credits: 10_000, status: 'ok', latencyMs: 541, error: null, batchIndex: null, root: null, commitTx: null },
      { id: 'u1', occurredAt: '14:21:54', keyId: '7Qk4', model: 'llama-3.2-3b', promptTokens: 100, cachedTokens: 0, completionTokens: 50, credits: 10_000, status: 'ok', latencyMs: 12700, error: null, batchIndex: null, root: null, commitTx: null },
      { id: 'u2', occurredAt: '14:21:31', keyId: '7Qk4', model: 'llama-3.2-3b', promptTokens: 100, cachedTokens: 0, completionTokens: 50, credits: 10_000, status: 'ok', latencyMs: null, error: null, batchIndex: null, root: null, commitTx: null },
    ];
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    fixture.detectChanges();

    const html = fixture.nativeElement.outerHTML;
    // Sub-second reads as milliseconds, a whole second up as seconds with
    // one decimal; absence is the tables' dash, never a fabricated zero.
    expect(html).toContain('ok · 541 ms');
    expect(html).toContain('ok · 12.7 s');
    expect(html).toContain('ok · -');
    expect(html).not.toContain('· 0 ms');
  });

  it('a filter that matches nothing renders No matching requests', async () => {
    setup();
    fakeApi.usageRows = [
      { id: 'u0', occurredAt: '14:20:12', keyId: '2Mv9', model: 'llama-3.2-3b', promptTokens: null, cachedTokens: null, completionTokens: null, credits: 0, status: 'error', latencyMs: null, error: '400 context_length', batchIndex: null, root: null, commitTx: null },
    ];
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    fixture.detectChanges();

    filterButtons()[1].click();
    fixture.detectChanges();

    expect(fixture.nativeElement.querySelector('table.tbl')).toBeNull();
    expect(fixture.nativeElement.outerHTML).toContain('No matching requests');
  });

  it('picking a key in the dropdown issues a new request with the prefix', async () => {
    setup();
    fakeApi.usageRows = [
      { id: 'u0', occurredAt: '14:22:07', keyId: '7Qk4', model: 'llama-3.2-3b', promptTokens: 8_412, cachedTokens: 6_144, completionTokens: 1_206, credits: 126_444, status: 'ok', latencyMs: 412, error: null, batchIndex: null, root: null, commitTx: null },
      { id: 'u1', occurredAt: '14:21:54', keyId: '2Mv9', model: 'qwen3-8-27b', promptTokens: 2_190, cachedTokens: 0, completionTokens: 884, credits: 48_420, status: 'ok', latencyMs: 391, error: null, batchIndex: null, root: null, commitTx: null },
    ];
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    fixture.detectChanges();
    const callsBefore = fakeApi.usageCalls;

    // Open the Key combobox and type to filter down to '2Mv9'; Enter picks it.
    const trigger = fixture.nativeElement.querySelector(
      '.log-filter-duo .sd-trigger',
    ) as HTMLElement;
    trigger.click();
    fixture.detectChanges();
    trigger.dispatchEvent(new KeyboardEvent('keydown', { key: '2' }));
    fixture.detectChanges();
    trigger.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter' }));
    fixture.detectChanges();
    await tick();

    expect(fakeApi.usageCalls).toBeGreaterThan(callsBefore);
    expect(fakeApi.lastUsageQuery?.key).toBe('2Mv9');
    expect(fakeApi.lastUsageQuery?.page).toBe(1);
  });

  it('offers exactly one All option per dropdown, selected by default', async () => {
    setup();
    fakeApi.usageRows = [
      { id: 'u0', occurredAt: '14:22:07', keyId: '7Qk4', model: 'llama-3.2-3b', promptTokens: 100, cachedTokens: 0, completionTokens: 50, credits: 10_000, status: 'ok', latencyMs: 412, error: null, batchIndex: null, root: null, commitTx: null },
      { id: 'u1', occurredAt: '14:21:54', keyId: '2Mv9', model: 'qwen3-8-27b', promptTokens: 100, cachedTokens: 0, completionTokens: 50, credits: 10_000, status: 'ok', latencyMs: 391, error: null, batchIndex: null, root: null, commitTx: null },
    ];
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    fixture.detectChanges();

    const triggers = Array.from(
      fixture.nativeElement.querySelectorAll('.log-filter-duo .sd-trigger'),
    ) as HTMLElement[];
    // The store starts on '' for both filters, so both triggers show their
    // placeholder labels.
    expect(triggers[0].textContent).toContain('All keys');
    expect(triggers[1].textContent).toContain('All models');

    triggers[0].click();
    fixture.detectChanges();
    const keyAll = Array.from(
      fixture.nativeElement.querySelectorAll('.sd-opt .opt-label'),
    ) as Element[];
    expect(keyAll.map((el) => el.textContent?.trim()).filter((t) => t === 'All keys')).toHaveLength(1);
    triggers[0].click();
    fixture.detectChanges();

    triggers[1].click();
    fixture.detectChanges();
    const modelAll = Array.from(
      fixture.nativeElement.querySelectorAll('.sd-opt .opt-label'),
    ) as Element[];
    expect(modelAll.map((el) => el.textContent?.trim()).filter((t) => t === 'All models')).toHaveLength(1);
  });

  it('lays out Key/Model on one row and the date pair on its own', () => {
    setup();
    // Key and Model are siblings inside a single .log-filter-duo.
    const duo = fixture.nativeElement.querySelector('.log-filter-duo') as Element;
    expect(duo).toBeTruthy();
    const fields = Array.from(duo.children);
    expect(fields.length).toBe(2);
    for (const f of fields) {
      expect(f.classList.contains('log-filter-field')).toBe(true);
      expect(f.querySelector('slf-search-select')).toBeTruthy();
    }
    // The From/To pair sits in its own row; the To label keeps its spacing
    // class so it does not crowd the From input.
    const dates = fixture.nativeElement.querySelector('.log-filter-dates') as Element;
    expect(dates).toBeTruthy();
    expect(dates.querySelector('#usageFrom')).toBeTruthy();
    expect(dates.querySelector('#usageTo')).toBeTruthy();
    const toLabel = dates.querySelector('label.log-filter-lbl-2') as HTMLElement;
    expect(toLabel.textContent?.trim()).toBe('To');
    // No native select remains on this screen.
    expect(duo.querySelector('select')).toBeNull();
  });

  it('the range control changes the chart, not the log fetch', async () => {
    setup();
    const callsBefore = fakeApi.usageCalls;
    const buttons = segButtons('Range');
    const sevenDay = buttons.find((b) => b.textContent?.trim() === '7 d');
    expect(sevenDay).toBeDefined();
    sevenDay!.click();
    fixture.detectChanges();
    await tick();

    // The range is a chart concern; it issues no new log request.
    expect(fakeApi.usageCalls).toBe(callsBefore);
    const store = TestBed.inject(ConsoleStore);
    expect(store.usageDays()).toBe(7);
  });

  it('shows the computed 30-day total from the fixture, not a magic number', () => {
    setup();
    TestBed.inject(ConsoleStore).usageDays.set(30);
    const component = fixture.componentInstance as unknown as {
      stats: () => { total: number } | null;
    };
    const expectedTotal = DAILY_SPEND.reduce((a, b) => a + b, 0);
    expect(component.stats()?.total).toBe(expectedTotal);
    fixture.detectChanges();
    expect(fixture.nativeElement.outerHTML).toContain(
      new Intl.NumberFormat('en-US').format(expectedTotal),
    );
  });

  it('switching the range to 7 d shortens the chart series', async () => {
    setup();
    TestBed.inject(ConsoleStore).usageDays.set(30);
    fixture.detectChanges();
    const component = fixture.componentInstance as unknown as {
      daily: () => number[];
      dayZero: () => number;
    };
    expect(component.daily().length).toBe(30);

    const buttons = segButtons('Range');
    buttons.find((b) => b.textContent?.trim() === '7 d')!.click();
    fixture.detectChanges();
    await tick();

    expect(component.daily().length).toBe(7);
    expect(component.dayZero()).toBeGreaterThan(0);
  });

  it('never renders NaN or Infinity in the stats panel', () => {
    setup();
    fixture.detectChanges();
    const html = fixture.nativeElement.outerHTML;
    expect(html).not.toContain('NaN');
    expect(html).not.toContain('Infinity');
  });

  it('shows which model each request used in the Model column', async () => {
    setup();
    fakeApi.usageRows = [
      { id: 'u0', occurredAt: '2026-08-25T14:22:07Z', keyId: '7Qk4', model: 'llama-3.2-3b', promptTokens: 8_412, cachedTokens: 6_144, completionTokens: 1_206, credits: 126_444, status: 'ok', latencyMs: 412, error: null, batchIndex: null, root: null, commitTx: null },
      { id: 'u1', occurredAt: '14:21:54', keyId: '7Qk4', model: 'qwen3-8-27b', promptTokens: 2_190, cachedTokens: 0, completionTokens: 884, credits: 48_420, status: 'ok', latencyMs: 391, error: null, batchIndex: null, root: null, commitTx: null },
    ];
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    fixture.detectChanges();

    const rows = Array.from(
      fixture.nativeElement.querySelectorAll('tbody tr'),
    ) as Element[];
    // The first two columns are Time and Key; the third is the Model.
    const models = rows.map((r) => r.querySelectorAll('td')[2]?.textContent?.trim());
    expect(models).toEqual(['llama-3.2-3b', 'qwen3-8-27b']);
    // The Time column renders through the shared local-time helper: the
    // wire's UTC instant in the runner's local zone, no suffix. The second
    // row's unparseable time-only value passes through unchanged.
    const times = rows.map((r) => r.querySelectorAll('td')[0]?.textContent?.trim());
    expect(times[0]).toBe(new Date('2026-08-25T14:22:07Z').toLocaleString());
    expect(times[1]).toBe('14:21:54');
  });

  it('scrolls the table in its own box, never the page', async () => {
    setup();
    fakeApi.usageRows = [
      { id: 'u0', occurredAt: '14:22:07', keyId: '7Qk4', model: 'llama-3.2-3b', promptTokens: 8_412, cachedTokens: 6_144, completionTokens: 1_206, credits: 126_444, status: 'ok', latencyMs: 412, error: null, batchIndex: null, root: null, commitTx: null },
    ];
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    fixture.detectChanges();

    const page = (fixture.nativeElement as HTMLElement).querySelector('section.page')!;
    expect(page).toBeTruthy();
    expect(page.classList).not.toContain('table-wrap');
    const table = page.querySelector('table.tbl')!;
    expect(table.closest('.table-wrap')).toBeTruthy();
    expect(table.parentElement!.classList).toContain('table-wrap');
  });

  function wireRow(
    id: string,
    error: string | null,
    keyId = '7Qk4',
  ): UsageRowT {
    return {
      id,
      occurredAt: '2026-08-25T14:22:07Z',
      keyId,
      model: 'llama-3.2-3b',
      promptTokens: 100,
      cachedTokens: 0,
      completionTokens: 50,
      credits: 10_000,
      status: error === null ? 'ok' : 'error',
      latencyMs: error === null ? 100 : null,
      error,
      batchIndex: null,
      root: null,
      commitTx: null,
    };
  }

  it('paginates the request log with numbered controls against the total', async () => {
    setup();
    fakeApi.usageRows = Array.from({ length: 60 }, (_, i) => wireRow(`u${i}`, null));
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    fixture.detectChanges();

    // 60 rows at 25 per page: three pages, page one showing the newest batch.
    expect(store.usage().map((r) => r.id)).toEqual(
      Array.from({ length: 25 }, (_, i) => `u${i}`),
    );
    const pager = fixture.nativeElement.querySelector('.pager') as Element;
    expect(pager).toBeTruthy();
    expect(pager.textContent).toContain('Page 1 of 3');
    const prev = fixture.nativeElement.querySelector('.pager .pager-btn') as HTMLButtonElement;
    const next = fixture.nativeElement.querySelectorAll('.pager .pager-btn')[1] as HTMLButtonElement;
    expect(prev.disabled).toBe(true);
    expect(next.disabled).toBe(false);

    next.click();
    fixture.detectChanges();
    await tick();
    expect(store.usagePage()).toBe(2);
    expect(store.usage().map((r) => r.id)).toEqual(
      Array.from({ length: 25 }, (_, i) => `u${25 + i}`),
    );
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('.pager').textContent).toContain('Page 2 of 3');

    prev.click();
    fixture.detectChanges();
    await tick();
    expect(store.usagePage()).toBe(1);
    fixture.detectChanges();
    expect((fixture.nativeElement.querySelector('.pager .pager-btn') as HTMLButtonElement).disabled).toBe(true);
  });

  it('a structured filter narrows the log and resets to page one', async () => {
    setup();
    fakeApi.usageRows = Array.from({ length: 60 }, (_, i) => wireRow(`u${i}`, null, i % 2 === 0 ? '7Qk4' : '2Mv9'));
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    store.gotoPage(3);
    await tick();
    expect(store.usagePage()).toBe(3);

    // Picking a key prefix is a new server query: back to page one, and the
    // matching total drops to that key's rows.
    store.setUsageKey('7Qk4');
    await tick();
    fixture.detectChanges();
    expect(store.usagePage()).toBe(1);
    expect(fakeApi.lastUsageQuery?.key).toBe('7Qk4');
    expect(fakeApi.lastUsageQuery?.page).toBe(1);
    expect(store.usageTotal()).toBe(30);
    expect(store.usage().length).toBe(25);
  });

  it('clears every filter in one action and returns to page one', async () => {
    setup();
    fakeApi.usageRows = Array.from({ length: 60 }, (_, i) => wireRow(`u${i}`, null));
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    store.setUsageKey('7Qk4');
    store.setUsageModel('llama-3.2-3b');
    await tick();
    store.gotoPage(2);
    await tick();
    expect(store.usagePage()).toBe(2);

    const clear = fixture.nativeElement.querySelector('.log-clear') as HTMLButtonElement;
    expect(clear).toBeTruthy();
    clear.click();
    fixture.detectChanges();
    await tick();

    expect(store.usageKey()).toBe('');
    expect(store.usageModel()).toBe('');
    expect(store.usageStatusFilter()).toBe('all');
    expect(store.usagePage()).toBe(1);
    expect(fakeApi.lastUsageQuery?.key).toBeUndefined();
    expect(fakeApi.lastUsageQuery?.model).toBeUndefined();
    // With no filter active the clear control is gone again.
    expect(fixture.nativeElement.querySelector('.log-clear')).toBeNull();
  });

  it('shows a filter-aware empty state when the filters match nothing', async () => {
    setup();
    fakeApi.usageRows = [wireRow('u0', null), wireRow('u1', null)];
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();

    // A key prefix with no rows yields an empty paged result; the empty
    // state names the active filter and offers a one-action clear.
    store.setUsageKey('nope');
    await tick();
    fixture.detectChanges();

    expect(store.usageTotal()).toBe(0);
    expect(fixture.nativeElement.querySelector('table.tbl')).toBeNull();
    const html = fixture.nativeElement.outerHTML;
    expect(html).toContain('No matching requests');
    expect(html).toContain('No requests match key nope.');
    expect(fixture.nativeElement.querySelector('.log-clear')).toBeTruthy();
  });

  // ---- settlement detail (EP-027-T05) ----------------------------------

  /** Load the mock's own fixture rows (with commitments) and open the
   *  detail row of `rowId`. Returns the detail cell. */
  async function openDetail(rowId: string): Promise<Element> {
    fakeApi.usageRows = (await new MockSlotfareApi().usage()).rows;
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    fixture.detectChanges();
    // The row's time cell is unique in the fixture, so it pins the row.
    const time = store.usage().find((r) => r.id === rowId)!.time;
    const rows = Array.from(
      fixture.nativeElement.querySelectorAll('tbody tr'),
    ) as Element[];
    const row = rows.find(
      (tr) => tr.querySelector('td[data-th="Time"]')?.textContent?.trim() === time,
    );
    expect(row).toBeTruthy();
    // The toggle lives in the Result cell of metered rows only.
    const toggle = row!.querySelector('.row-toggle') as HTMLButtonElement;
    expect(toggle).toBeTruthy();
    toggle.click();
    fixture.detectChanges();
    return document.getElementById(`usage-detail-${rowId}`)!;
  }

  it('a settled row expands to its commitment: batch index, truncated root with copy, explorer link', async () => {
    setup();
    const detail = await openDetail('u1');
    expect(detail).toBeTruthy();
    const text = detail.textContent ?? '';
    // The batch index renders plain; the root truncated, with the full value
    // on the copy button.
    expect(text).toContain('Batch index');
    expect(text).toContain('12');
    const row = TestBed.inject(ConsoleStore).usage().find((r) => r.id === 'u1')!;
    const commitment = row.commitment!;
    expect(text).toContain(short(commitment.root));
    expect(detail.querySelector('slf-copy-button')).toBeTruthy();
    // The commit transaction opens in the Solana explorer: usage roots
    // settle as memo transactions, ending in /tx/<commitTx>.
    const link = detail.querySelector('a.xlink[target="_blank"]') as HTMLAnchorElement;
    expect(link).toBeTruthy();
    expect(link.getAttribute('href')!.startsWith('https://explorer.solana.com/tx/')).toBe(true);
    expect(link.getAttribute('href')).toContain(`/tx/${commitment.commitTx}`);
    expect(link.title).toContain('Solana explorer');
    // The manual on-chain root check is documented in place.
    expect(text).toContain('manual step');
    // The verify affordance is offered for a settled row.
    expect(detail.querySelector('.usage-verify .btn')).toBeTruthy();
    // Error rows get no expand affordance at all.
    const errorRow = Array.from(
      fixture.nativeElement.querySelectorAll('tbody tr'),
    )
      .filter((tr): tr is Element => tr instanceof Element)
      .find((tr) => tr.querySelector('.pill-alert') !== null && !tr.classList.contains('usage-detail'));
    expect(errorRow?.querySelector('.row-toggle')).toBeNull();
  });

  it('a pending row says awaiting settlement plainly and offers no verify', async () => {
    setup();
    const detail = await openDetail('u0');
    expect(detail).toBeTruthy();
    const text = detail.textContent ?? '';
    expect(text).toContain('Awaiting settlement');
    expect(detail.querySelector('.usage-verify')).toBeNull();
    expect(detail.querySelector('slf-copy-button')).toBeNull();
    expect(detail.querySelector('a.xlink')).toBeNull();
  });

  it('a verify click walks the fixture proof and renders the verified pill', async () => {
    setup();
    const detail = await openDetail('u1');
    const verify = detail.querySelector('.usage-verify .btn') as HTMLButtonElement;
    verify.click();
    await tick();
    fixture.detectChanges();
    expect(detail.querySelector('.pill-ok')).toBeTruthy();
    expect(detail.textContent).toContain('Verified');
  });

  it('a verify click renders the failed pill when the served proof is corrupted', async () => {
    setup();
    const detail = await openDetail('u1');
    fakeApi.tamperProofNext = true;
    const verify = detail.querySelector('.usage-verify .btn') as HTMLButtonElement;
    verify.click();
    await tick();
    fixture.detectChanges();
    expect(detail.querySelector('.pill-alert')).toBeTruthy();
    expect(detail.textContent).toContain('does not match');
  });
});

describe('UsageComponent in live mode (no mock flag)', () => {
  // Flip the mock flag off for these cases so the chart can only ever be
  // fed by the store's real rows. The stream gate is environment.useMock,
  // so stub EventSource (jsdom has none) exactly like console-store.spec.
  const useMockBefore = environment.useMock;
  class StubEventSource {
    onopen: (() => void) | null = null;
    onerror: (() => void) | null = null;
    constructor(_url: string) {}
    addEventListener(): void {}
    removeEventListener(): void {}
    close(): void {}
  }

  beforeAll(() => {
    vi.stubGlobal('EventSource', StubEventSource);
    environment.useMock = false;
  });
  afterAll(() => {
    vi.unstubAllGlobals();
    environment.useMock = useMockBefore;
  });

  let fixture: ComponentFixture<UsageComponent>;
  let fakeApi: FakeApi;
  let fakeWallet: WalletService;

  function setup() {
    fakeApi = new FakeApi();
    fakeWallet = makeFakeWallet();
    TestBed.configureTestingModule({
      imports: [UsageComponent],
      providers: [
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: fakeWallet },
        ConsoleStore,
        ToastService,
      ],
    });
    fixture = TestBed.createComponent(UsageComponent);
    fixture.detectChanges();
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
  }

  afterEach(() => TestBed.inject(ConsoleStore).stopPolling());

  it('shows the empty state instead of the fixture when there are no rows', async () => {
    setup();
    await tick();
    fixture.detectChanges();

    // The chart is replaced by the same empty state as the request log.
    expect(fixture.nativeElement.querySelector('slf-spend-chart')).toBeNull();
    const html = fixture.nativeElement.outerHTML;
    expect(html).toContain('No usage yet');
    // Neither fixture series is reachable without the mock flag.
    const spendTotal = new Intl.NumberFormat('en-US').format(
      DAILY_SPEND.reduce((a, b) => a + b, 0),
    );
    const requestsTotal = new Intl.NumberFormat('en-US').format(
      DAILY_REQUESTS.reduce((a, b) => a + b, 0),
    );
    expect(html).not.toContain(spendTotal);
    expect(html).not.toContain(requestsTotal);
  });

  it('buckets real rows into per-day series and totals their credits', async () => {
    setup();
    const now = Date.now();
    const wire = (id: string, daysAgo: number, credits: number): UsageRowT => ({
      id,
      occurredAt: new Date(now - daysAgo * DAY_MS).toISOString(),
      keyId: '7Qk4',
      model: 'llama-3.2-3b',
      promptTokens: 100,
      cachedTokens: 0,
      completionTokens: 50,
      credits: credits * 10_000,
      status: 'ok',
      latencyMs: 100,
      error: null,
      batchIndex: null,
      root: null,
      commitTx: null,
    });
    fakeApi.usageRows = [
      wire('u0', 1, 100),
      wire('u1', 3, 50),
      wire('u2', 5, 25),
    ];
    const store = TestBed.inject(ConsoleStore);
    await store.loadUsage();
    fixture.detectChanges();

    const component = fixture.componentInstance as unknown as {
      daily: () => number[];
      stats: () => { total: number } | null;
    };
    // A 7-day window: index 6 is today, so yesterday is 5, three days
    // ago is 3, five days ago is 1; every other day is zero.
    const expected = new Array<number>(7).fill(0);
    expected[5] = 100;
    expected[3] = 50;
    expected[1] = 25;
    expect(component.daily()).toEqual(expected);
    expect(component.stats()?.total).toBe(175);
    // The chart renders the real bars, not the empty state.
    expect(fixture.nativeElement.querySelector('slf-spend-chart')).toBeTruthy();
    expect(fixture.nativeElement.outerHTML).not.toContain('No usage yet');
  });

  it('shortens the series to the selected range in live mode', async () => {
    setup();
    TestBed.inject(ConsoleStore).usageDays.set(30);
    fixture.detectChanges();
    const component = fixture.componentInstance as unknown as {
      daily: () => number[];
    };
    expect(component.daily().length).toBe(30);

    const group = fixture.nativeElement.querySelector(
      '.seg[aria-label="Range"]',
    ) as Element;
    const buttons = Array.from(group.querySelectorAll('button')) as HTMLButtonElement[];
    const sevenDay = buttons.find((b) => b.textContent?.trim() === '7 d')!;
    sevenDay.click();
    fixture.detectChanges();
    await tick();

    expect(component.daily().length).toBe(7);
  });
});
