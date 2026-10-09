import { computed, signal } from '@angular/core';
import { readFileSync } from 'node:fs';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { ToastService } from '../core/toast';
import {
  Account, ApiError, AttestChain, ChainInfo,
  CreatedKey, DepositTarget, FundingStatus, Key,
  ListingPage, MarketConfirmBody, MarketPrepareBody, ModelList,
  Nonce, PreparedMarketTx, SLOTFARE_API, SlotfareApi,
  UsagePage, UsageProof, UsageQuery, VerifyResult,
} from '../core/api';
import { ConsoleSession, WalletService } from '../core/wallet';
import { Listing, MarketListing, Model, Wallet } from '../core/models';
import { environment } from '../../environments/environment';
import { ModelsComponent } from './models.component';

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
  /** Mutable catalog returned by models(). */
  modelData: Model[] = [];
  /** When set, the next call to models() rejects with it once. */
  failNextModels: ApiError | null = null;
  /** Counts how many times models() has been called. */
  modelsCalls = 0;
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
      balanceCredits: 0,
      lockedCredits: 0,
      totalSpentCredits: 0,
      createdAt: '2026-06-14',
      baseUrl: 'https://wire.example/v1',
    };
  }
  async keys(): Promise<Key[]> {
    return [];
  }
  async createKey(_label: string): Promise<CreatedKey> {
    throw new ApiError(500, 'internal', 'no');
  }
  async revokeKey(_id: string): Promise<void> {}
  async usage(_query?: UsageQuery): Promise<UsagePage> {
    return { rows: [], nextCursor: null };
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
    this.modelsCalls += 1;
    if (this.failNextModels !== null) {
      const e = this.failNextModels;
      this.failNextModels = null;
      throw e;
    }
    return { models: this.modelData.map((m) => ({ ...m })), baseUrl: '' };
  }

  /** EP-027-T05: the inclusion proof read; this screen does not exercise it. */
  async usageProof(_rowId: string): Promise<UsageProof> {
    throw new ApiError(500, 'internal', 'not implemented');
  }
}

/** Six wire shapes: every metadata field the catalog can carry. */
const RICH_MODELS: Model[] = [
  { id: 'mistral-nemotron', name: 'Mistral Nemotron', vendor: 'Mistral', contextLength: 131072, capabilities: ['function-calling'], description: 'A 12B reasoning model with strong tool use.', pricing: { uncachedInput: 20, cachedInput: 2, output: 30 } },
  { id: 'diffusiongemma-26b', name: 'Google DiffusionGemma 26B A4B', vendor: 'Google', contextLength: 32768, servingPath: 'priority', description: 'Fast 26B mixture-of-experts chat model.', pricing: { uncachedInput: 50, cachedInput: 5, output: 250 } },
  { id: 'llama-3.2-3b', name: 'Llama 3.2 3B', vendor: 'Meta', contextLength: 32768, parameters: '3B', servingPath: 'fast', capabilities: ['vision'], pricing: { uncachedInput: 10, cachedInput: 1, output: 40 } },
  { id: 'qwen3-8-27b', name: 'Qwen3 27B', vendor: 'Alibaba', contextLength: 262144, parameters: '27B', servingPath: 'standard', capabilities: ['function-calling'], pricing: { uncachedInput: 100, cachedInput: 10, output: 300 } },
  { id: 'jina-v3-embeddings', name: 'Jina v3 Embeddings', kind: 'embeddings', vendor: 'Jina', contextLength: 524288, description: 'Multilingual text embeddings.', pricing: { uncachedInput: 50, cachedInput: 0, output: 0 } },
  { id: 'rerank-v1', name: 'Slotfare Reranker v1', kind: 'reranker', pricing: { uncachedInput: 20, cachedInput: 0, output: 0 } },
];

/** The two bare testnet models: name, id, pricing and nothing else. */
const TESTNET_TWO: Model[] = [
  { id: 'llama-3.2-3b', name: 'Llama 3.2 3B', pricing: { uncachedInput: 10, cachedInput: 1, output: 40 } },
  { id: 'qwen3-8-27b', name: 'Qwen3 27B', pricing: { uncachedInput: 100, cachedInput: 10, output: 300 } },
];

describe('ModelsComponent', () => {
  beforeAll(() => {
    environment.useMock = true;
  });
  let fixture: ComponentFixture<ModelsComponent>;
  let fakeApi: FakeApi;

  function setup(modelData: Model[] = []) {
    fakeApi = new FakeApi();
    fakeApi.modelData = modelData;
    TestBed.configureTestingModule({
      imports: [ModelsComponent],
      providers: [
        provideRouter([]),
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: makeFakeWallet() },
        ConsoleStore,
        ToastService,
      ],
    });
    fixture = TestBed.createComponent(ModelsComponent);
    fixture.detectChanges();
    const wallet = TestBed.inject(WalletService);
    wallet.address.set('0xabc');
    wallet.accountId.set('0xmockacct');
    wallet.token.set('mock-token');
    TestBed.flushEffects();
  }

  afterEach(() => TestBed.inject(ConsoleStore).stopPolling());

  function el(): HTMLElement {
    return fixture.nativeElement as HTMLElement;
  }

  it('renders every wire model as a linkable row', async () => {
    setup(RICH_MODELS);
    await tick();
    fixture.detectChanges();

    const rows = Array.from(el().querySelectorAll('a.model-row'));
    expect(rows.length).toBe(RICH_MODELS.length);
    // Each row is a real link to its detail route; the id no longer
    // renders on the card, so the row is found by its href.
    for (const m of RICH_MODELS) {
      const row = rows.find((r) => r.getAttribute('href') === `/models/${m.id}`);
      expect(row).toBeDefined();
      expect(row!.textContent).toContain(m.name);
    }
    // The stacked pricing column carries the wire's per-1M figures.
    const llama = rows.find((r) => r.getAttribute('href') === '/models/llama-3.2-3b')!;
    expect(llama.textContent).toContain('10 credits / M');
    expect(llama.textContent).toContain('uncached');
  });

  it('derives vendor chips with counts and no context buckets', async () => {
    setup(RICH_MODELS);
    await tick();
    fixture.detectChanges();

    // Five vendors plus `other` for the vendor-less reranker, count desc,
    // each shown by its display name (alibaba -> Qwen, mistral -> Mistral AI).
    const chips = Array.from(el().querySelectorAll('.model-chip')).map((c) =>
      c.textContent?.trim(),
    );
    expect(chips).toEqual([
      'Qwen (1)',
      'Google (1)',
      'Jina (1)',
      'Meta (1)',
      'Mistral AI (1)',
      'Other (1)',
    ]);
    // Only the Vendor axis: no Context, Type or Serving path filter rows.
    const rowLabels = Array.from(el().querySelectorAll('.model-filter-lbl')).map(
      (l) => l.textContent?.trim(),
    );
    expect(rowLabels).toEqual(['Vendor']);
  });

  it('vendor filter and search compose', async () => {
    setup(RICH_MODELS);
    await tick();
    fixture.detectChanges();

    const search = el().querySelector<HTMLInputElement>('.model-search')!;
    const chips = () =>
      Array.from(el().querySelectorAll('.model-chip')) as HTMLElement[];

    // Search "b" hits many names; add the Meta vendor and only Meta's
    // matching model survives.
    search.value = 'b';
    search.dispatchEvent(new Event('input'));
    fixture.detectChanges();
    expect(el().querySelectorAll('.model-row').length).toBeGreaterThan(1);

    const meta = chips().find((c) => c.textContent?.trim().startsWith('Meta'))!;
    meta.click();
    fixture.detectChanges();
    expect(el().querySelectorAll('.model-row').length).toBe(1);
    // The id no longer renders on the card; the surviving row is Meta's.
    expect(el().querySelector('.model-row')!.getAttribute('href')).toBe(
      '/models/llama-3.2-3b',
    );

    // Clear all restores the full search result.
    (el().querySelector('.model-filter-active .btn') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(search.value).toBe('');
    expect(el().querySelectorAll('.model-row').length).toBeGreaterThan(1);
  });

  it('puts the active-filter read on the vendor row, not on its own line', async () => {
    setup(RICH_MODELS);
    await tick();
    fixture.detectChanges();

    // No filters: no read at all, in either place.
    expect(el().querySelector('.model-filter-active')).toBeNull();
    expect(el().querySelector('.model-active')).toBeNull();

    const meta = Array.from(el().querySelectorAll('.model-chip'))
      .find((c) => c.textContent?.trim().startsWith('Meta')) as HTMLElement;
    meta.click();
    fixture.detectChanges();

    // The single-filter read sits inside the vendor filter row, with the
    // clear action, and the standalone block is gone.
    const row = el().querySelector('.model-filter-row')!;
    const read = row.querySelector('.model-filter-active')!;
    expect(read.textContent).toContain('1 filter active');
    expect(read.querySelector('button.btn-ghost')).not.toBeNull();
    expect(el().querySelector('.model-active')).toBeNull();

    // Two active filters pluralise the read.
    const search = el().querySelector<HTMLInputElement>('.model-search')!;
    search.value = 'b';
    search.dispatchEvent(new Event('input'));
    fixture.detectChanges();
    expect(el().querySelector('.model-filter-row .model-filter-active')!.textContent)
      .toContain('2 filters active');
  });

  it('absent metadata renders nothing; bare rows stay complete', async () => {
    setup(TESTNET_TWO);
    await tick();
    fixture.detectChanges();

    expect(el().querySelectorAll('.model-row').length).toBe(2);
    // No vendor tags, no description, no context line: the wire carries none.
    expect(el().querySelector('.model-vendor')).toBeNull();
    expect(el().querySelector('.model-context')).toBeNull();
    expect(el().textContent).not.toContain('Context');
    // Both rows still carry name and the stacked pricing column.
    for (const m of TESTNET_TWO) {
      const row = el().querySelectorAll('.model-row')[TESTNET_TWO.indexOf(m)]!;
      expect(row.textContent).toContain(m.name);
      expect(row.querySelector('.model-pricing .model-price-val')).not.toBeNull();
    }
    const qwen = el().querySelectorAll('.model-row')[1]!;
    expect(qwen.textContent).toContain('100 credits / M');
  });

  it('shows the vendor and humanized context on one line when the wire carries them', async () => {
    setup(RICH_MODELS);
    await tick();
    fixture.detectChanges();

    const row = (href: string) =>
      el().querySelector(`a.model-row[href="${href}"]`)!;
    const line = (href: string) => row(href).querySelector('.model-context')!;
    // Vendor display name, a separator, then the context.
    expect(line('/models/qwen3-8-27b').querySelector('.model-context-vendor')!.textContent).toBe('Qwen');
    expect(line('/models/qwen3-8-27b').textContent).toContain('Context 262k');
    expect(line('/models/jina-v3-embeddings').textContent).toContain('Context 524k');
    // The reranker carries neither vendor nor contextLength: no line at all.
    expect(row('/models/rerank-v1').querySelector('.model-context')).toBeNull();
  });

  it('stacks the pricing column per kind', async () => {
    setup(RICH_MODELS);
    await tick();
    fixture.detectChanges();

    const lines = (href: string): string[] =>
      Array.from(
        el().querySelector(`a.model-row[href="${href}"]`)!.querySelectorAll('.model-price-line'),
      ).map((l) => `${l.querySelector('.model-price-val')!.textContent?.trim()} ${l.querySelector('.model-price-lbl')!.textContent?.trim()}`);

    // Chat: three lines in order, value then muted label.
    expect(lines('/models/mistral-nemotron')).toEqual([
      '20 credits / M uncached',
      '2 credits / M cached',
      '30 credits / M output',
    ]);
    // Embeddings and rerankers: their single input line only.
    expect(lines('/models/jina-v3-embeddings')).toEqual(['50 credits / M uncached']);
    expect(lines('/models/rerank-v1')).toEqual(['20 credits / M uncached']);
  });

  it('leads each card with a bundled logo or the letter avatar fallback', async () => {
    setup(RICH_MODELS);
    await tick();
    fixture.detectChanges();

    const logo = (href: string) =>
      el().querySelector(`a.model-row[href="${href}"]`)!.querySelector('.model-logo')!;

    // Display-name vendors resolve to their bundled slugs.
    expect(logo('/models/mistral-nemotron').querySelector('img.model-logo-img')!.getAttribute('src'))
      .toBe('vendor-logos/mistralai.svg');
    expect(logo('/models/qwen3-8-27b').querySelector('img.model-logo-img')!.getAttribute('src'))
      .toBe('vendor-logos/qwen.svg');
    // The google display name hits the bundled google mark, not the letter.
    const gemmaImg = logo('/models/diffusiongemma-26b').querySelector('img.model-logo-img');
    expect(gemmaImg!.getAttribute('src')).toBe('vendor-logos/google.svg');
    expect(logo('/models/diffusiongemma-26b').querySelector('.avatar')).toBeNull();
    // Unknown vendor and vendor-less rows fall back to the letter avatar.
    const jinaAvatar = logo('/models/jina-v3-embeddings').querySelector('.avatar');
    expect(jinaAvatar!.textContent).toBe('J');
    expect(logo('/models/jina-v3-embeddings').querySelector('img')).toBeNull();
    const rerankAvatar = logo('/models/rerank-v1').querySelector('.avatar');
    expect(rerankAvatar!.textContent).toBe('S');
    expect(logo('/models/rerank-v1').querySelector('img')).toBeNull();
  });

  it('shows the description on the list cards, clamped by CSS', async () => {
    setup(RICH_MODELS);
    await tick();
    fixture.detectChanges();

    expect(el().querySelector('.model-desc')).not.toBeNull();
    expect(el().textContent).toContain('A 12B reasoning model');
  });

  it('highlights the card on hover, not its text', async () => {
    setup([]);
    // The shipped stylesheet is the claim: read it like funding-trace does.
    const cssPath = import.meta.url
      .replace(/app\/screens\/[^/]+$/, 'styles/slotfare.scss')
      .replace(/^file:\/\//, '');
    const css = readFileSync(cssPath, 'utf8');

    const hoverRule = css.match(/\.model-row:hover\s*\{[^}]*\}/)![0];
    // The hover floods the card with the acid accent and lifts it on a hard
    // offset shadow, the print-language equivalent of a glow ring.
    expect(hoverRule).toContain('--acid');
    expect(hoverRule).toContain('--hard');
    // text-decoration inherits, so the card's hover state must explicitly
    // cancel the global a:hover underline at the boundary or every label
    // inside would underline on hover, even though the hover may retint the
    // card's own text.
    expect(hoverRule).toContain('text-decoration: none');
    // The anchor never tints its text in the resting state.
    const rowRule = css.match(/\.model-row\s*\{[^}]*\}/)![0];
    expect(rowRule).toContain('color: inherit');
    // No hover rule targets the card's text spans.
    for (const sel of ['.model-name:hover', '.model-meta:hover', '.model-price-lbl:hover']) {
      expect(css).not.toContain(sel);
    }
  });

  it('renders the card grid two columns on desktop, one below', async () => {
    setup([]);
    const cssPath = import.meta.url
      .replace(/app\/screens\/[^/]+$/, 'styles/slotfare.scss')
      .replace(/^file:\/\//, '');
    const css = readFileSync(cssPath, 'utf8');

    // Desktop pinned breakpoint: two columns, so even a single card takes
    // one column, not a full-width strip.
    const media = css.match(/@media \(min-width: \$fb-tablet \+ 1px\)\s*\{[\s\S]*?\n\}/)![0];
    expect(media).toContain('.model-list-body');
    expect(media).toContain('repeat(2, minmax(0, 1fr))');
    // Below desktop the grid stays one column.
    const base = css.match(/\.model-list-body\s*\{[^}]*\}/)![0];
    expect(base).toContain('grid-template-columns: 1fr');
  });

  it('renders the vendor mark as a fixed ink tile with breathing room', async () => {
    setup([]);
    const cssPath = import.meta.url
      .replace(/app\/screens\/[^/]+$/, 'styles/slotfare.scss')
      .replace(/^file:\/\//, '');
    const css = readFileSync(cssPath, 'utf8');

    // A square 48px tile on the brand ink in both themes: the marks are one light glyph.
    const logoRule = css.match(/\.model-logo\s*\{[^}]*\}/)![0];
    expect(logoRule).toContain('aspect-ratio');
    expect(logoRule).toContain('background: var(--brand-ink)');
    expect(logoRule).not.toContain('height: 28px');
    // The mark sits inside the tile with padding around it, never edge to edge.
    const imgRule = css.match(/\.model-logo-img\s*\{[^}]*\}/)![0];
    expect(imgRule).toContain('width: 58%');
    expect(imgRule).toContain('object-fit: contain');
  });

  it('sits the vendor filter label at the standard label-to-control spacing', async () => {
    setup([]);
    const cssPath = import.meta.url
      .replace(/app\/screens\/[^/]+$/, 'styles/slotfare.scss')
      .replace(/^file:\/\//, '');
    const css = readFileSync(cssPath, 'utf8');

    const lblRule = css.match(/\.model-filter-lbl\s*\{[^}]*\}/)![0];
    expect(lblRule).not.toContain('min-width: 92px');
    expect(lblRule).toContain('min-width: 44px');
  });

  it('an empty catalog says so without inventing filters', async () => {
    setup([]);
    await tick();
    fixture.detectChanges();

    expect(el().textContent).toContain('No models available.');
    expect(el().querySelectorAll('.model-row').length).toBe(0);
    expect(el().querySelectorAll('.model-chip').length).toBe(0);
  });

  it('a failed load shows the error block and its retry re-fetches', async () => {
    setup();
    const store = TestBed.inject(ConsoleStore);
    fakeApi.failNextModels = new ApiError(503, 'unavailable', 'nope');
    await store.loadModels();
    fixture.detectChanges();

    expect(el().querySelector('slf-error-block')).not.toBeNull();
    expect(el().querySelector('slf-skeleton')).toBeNull();
    const before = fakeApi.modelsCalls;
    (el().querySelector('slf-error-block button.btn-secondary') as HTMLButtonElement).click();
    await tick();
    fixture.detectChanges();
    expect(fakeApi.modelsCalls).toBeGreaterThan(before);
  });
});
