import { computed, signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter, ActivatedRoute } from '@angular/router';
import { of } from 'rxjs';
import { readFileSync } from 'node:fs';
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
import { mid, short } from '../core/trace';
import { FundingTraceComponent } from './funding-trace.component';

const tick = (): Promise<void> => new Promise((r) => setTimeout(r));

/**
 * The trace detail rows (`.step-meta` cells) are the one shared key-value
 * rendering whose label and value were separated only by a literal HTML
 * space, so `Transaction` ran straight into its hash. The fix makes each
 * cell a flex pair with a CSS gap (the same mechanism `.kv` already uses),
 * and the template carries no space of its own. This spec renders the real
 * trace through the real stylesheet and pins both halves: label and value
 * are distinct elements, and their separation is a CSS gap.
 */

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

/** A settled direct purchase: the one-step trace with Transaction / Block / Credited rows. */
const DIRECT_PURCHASE: FundingStatus = {
  purchaseId: 'p1',
  state: 'settled',
  ethTx: '',
  ethBlock: 0,
  attestedHeight: null,
  chainTip: 0,
  builderCeiling: null,
  ccTx: '0xc607096c1d3a2f8b4e90517c2a6f83d0b94c5e718a3f2d6c4b0a9e5f1d2b653e0',
  ccBlock: 5372014,
  credits: 5000,
  amountUsd: 250,
  token: 'mUSDC',
  startedAt: '2026-08-25T10:00:00Z',
  updatedAt: '2026-08-25T10:01:00Z',
  chainKey: 0,
  chainName: 'Solana',
};

/** A settled proven purchase on Ethereum: the three-step proof anatomy, from
 *  the deposit seen at block 100 to the attested height to the mint. */
const ATTESTED_PURCHASE: FundingStatus = {
  purchaseId: 'p9',
  state: 'settled',
  ethTx: '0x1f8a2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9',
  ethBlock: 100,
  attestedHeight: 100,
  chainTip: 150,
  builderCeiling: 100,
  ccTx: '0xdeadbeef0011223344556677889900aabbccddeeff0011223344556677889900aa',
  ccBlock: 5372015,
  credits: 5000,
  amountUsd: 250,
  token: 'mUSDC',
  startedAt: '2026-08-25T10:00:00Z',
  updatedAt: '2026-08-25T10:07:00Z',
  chainKey: 1,
  chainName: 'Ethereum',
};

/** The Ethereum row the store's traceSourceExplorer resolves against. */
const ETHEREUM_CHAIN: ChainInfo = {
  chainKey: 1,
  name: 'Ethereum',
  chainId: 11155111,
  deposit: '0x0000000000000000000000000000000000000001',
  explorer: 'https://ethereum-scan.example.com',
};

/** A pending Ethereum purchase whose mint has not landed yet: the deposit
 *  fixes both figures, so the card derives them from the amount. */
const PENDING_PURCHASE: FundingStatus = {
  purchaseId: 'p2',
  state: 'pending',
  ethTx: '0x2b9f4e7d8a1c3f5b6e0d2a4c8f1e3b5d7a9c1e3f5b7d9a2c4e6f8b0d2a4c6e8f',
  ethBlock: null,
  attestedHeight: null,
  chainTip: null,
  builderCeiling: null,
  ccTx: null,
  ccBlock: null,
  credits: null,
  amountUsd: 250,
  token: 'mUSDC',
  startedAt: '2026-08-25T10:00:00Z',
  updatedAt: '2026-08-25T10:00:00Z',
  chainKey: 1,
  chainName: 'Ethereum',
};

class DirectPurchaseApi implements SlotfareApi {
  /** When set, funding() / fundingList() serve the attested proven
   *  purchase and chains() serves its Ethereum source row. */
  attested = false;
  /** An optional extra purchase entry; when set it is served alongside the
   *  primary fixture (primary first, so it stays the default pick). */
  pending: FundingStatus | null = null;
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
      balanceCredits: 5_000,
      lockedCredits: 0,
      totalSpentCredits: 0,
      createdAt: '2026-06-14',
      baseUrl: '',
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
    return this.attested ? { ...ATTESTED_PURCHASE } : { ...DIRECT_PURCHASE };
  }
  async fundingList(): Promise<FundingStatus[]> {
    const primary = this.attested ? ATTESTED_PURCHASE : DIRECT_PURCHASE;
    return this.pending !== null ? [{ ...primary }, { ...this.pending }] : [{ ...primary }];
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
    if (!this.attested) return [];
    return [ETHEREUM_CHAIN];
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

describe('FundingTraceComponent step-meta rows', () => {
  // The store's stream gate is environment.useMock; keep it true so
  // these specs never open a real EventSource (jsdom has none).
  beforeAll(() => {
    environment.useMock = true;
    // Load the real shared stylesheet into the test DOM so the
    // computed-gap assertion exercises the rule that ships. The runner
    // exposes import.meta.url as a file URL, and the stylesheet sits at
    // src/styles/slotfare.scss next to this spec's src/app/screens/ directory.
    const cssPath = import.meta.url.replace(/app\/screens\/[^/]+$/, 'styles/slotfare.scss').replace(/^file:\/\//, '');
    const css = readFileSync(cssPath, 'utf8');
    const style = document.createElement('style');
    style.textContent = css;
    document.head.appendChild(style);
  });

  let fakeWallet: WalletService;
  let fakeApi: DirectPurchaseApi;

  function setup(id: string | null = null, attested = false): ComponentFixture<FundingTraceComponent> {
    fakeWallet = makeFakeWallet();
    fakeApi = new DirectPurchaseApi();
    fakeApi.attested = attested;
    TestBed.configureTestingModule({
      imports: [FundingTraceComponent],
      providers: [
        provideRouter([]),
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: fakeWallet },
        ConsoleStore,
        ToastService,
        {
          provide: ActivatedRoute,
          useValue: {
            paramMap: of({ get: () => id }),
            snapshot: { paramMap: { get: () => id } },
          },
        },
      ],
    });
    const fixture = TestBed.createComponent(FundingTraceComponent);
    fixture.detectChanges();
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
    return fixture;
  }

  /** The trace root as a DOM element, following the repo's spec convention. */
  function root(fixture: ComponentFixture<FundingTraceComponent>): HTMLElement {
    return fixture.nativeElement as unknown as HTMLElement;
  }

  /** The proven-purchase variant of the setup: the attested fixture and its
   *  Ethereum source chain, so the source-explorer link resolves. */
  function setupAttested(id: string | null = null): ComponentFixture<FundingTraceComponent> {
    return setup(id, true);
  }

  /** The pending-purchase variant: the pre-mint fixture as the newest row
   *  plus its Ethereum source chain. */
  function setupPending(): ComponentFixture<FundingTraceComponent> {
    fakeWallet = makeFakeWallet();
    fakeApi = new DirectPurchaseApi();
    fakeApi.attested = true;
    fakeApi.pending = PENDING_PURCHASE;
    TestBed.configureTestingModule({
      imports: [FundingTraceComponent],
      providers: [
        provideRouter([]),
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: fakeWallet },
        ConsoleStore,
        ToastService,
        {
          provide: ActivatedRoute,
          useValue: {
            paramMap: of({ get: () => null }),
            snapshot: { paramMap: { get: () => null } },
          },
        },
      ],
    });
    const fixture = TestBed.createComponent(FundingTraceComponent);
    fixture.detectChanges();
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    TestBed.flushEffects();
    return fixture;
  }

  /** The Purchase card's kv rows: label to value-cell text. */
  function kvRows(fixture: ComponentFixture<FundingTraceComponent>): Map<string, HTMLElement> {
    const rows = Array.from(root(fixture).querySelectorAll<HTMLElement>('.kv-list .kv'));
    const map = new Map<string, HTMLElement>();
    for (const r of rows) {
      const label = r.querySelector('span')?.textContent ?? '';
      if (label !== '' && !map.has(label)) map.set(label, r);
    }
    return map;
  }

  afterEach(() => TestBed.inject(ConsoleStore).stopPolling());

  it('renders every trace detail row as distinct label and value elements', async () => {
    const fixture = setup();
    await tick();
    fixture.detectChanges();
    const cells = root(fixture).querySelectorAll('.step-meta > div');
    expect(cells.length).toBeGreaterThan(0);
    for (const cell of Array.from(cells)) {
      const label = cell.querySelector('span');
      const value = cell.querySelector('b');
      expect(label).not.toBeNull();
      expect(value).not.toBeNull();
      expect(label).not.toBe(value);
      // Neither element's text swallows the other's: no amount of
      // whitespace collapse can merge two distinct nodes.
      expect(label!.textContent).not.toContain(value!.textContent);
    }
  });

  it('separates label and value with a CSS gap, not a literal space', async () => {
    const fixture = setup();
    await tick();
    fixture.detectChanges();
    const cells = root(fixture).querySelectorAll('.step-meta > div');
    expect(cells.length).toBeGreaterThan(0);
    for (const cell of Array.from(cells)) {
      const computed = window.getComputedStyle(cell);
      expect(computed.display).toBe('flex');
      expect(computed.gap).toBe('6px');
      // The rendered markup carries no separating whitespace between the
      // two elements; the gap is the single source of truth.
      const source = cell.innerHTML;
      expect(source).toContain('</span><b>');
      expect(source).not.toContain('</span> <b>');
    }
  });

  it('caps the wallet and account addresses at 18 chars, middle-truncated', async () => {
    const fullAddress = '0x1234567890abcdef1234567890abcdef12345678';
    const fixture = setup('p1');
    fakeWallet.address.set(fullAddress);
    await TestBed.inject(ConsoleStore).startPolling();
    await tick();
    fixture.detectChanges();
    const rows = Array.from(root(fixture).querySelectorAll('.kv-list .kv'));
    const valueOf = (label: string) =>
      rows.find((r) => r.querySelector('span')?.textContent === label)?.querySelector('b')?.textContent;
    expect(valueOf('From wallet')).toBe(mid(fullAddress, 18));
    expect(valueOf('Credit account')).toBe(mid(fullAddress, 18));
    // The raw 42-char address never reaches the page.
    expect(root(fixture).textContent).not.toContain(fullAddress);
  });

  it('renders the Started fact in the viewer\'s local zone, without a UTC suffix', async () => {
    const fixture = setup('p1');
    await TestBed.inject(ConsoleStore).startPolling();
    await tick();
    fixture.detectChanges();
    const rows = Array.from(root(fixture).querySelectorAll('.kv-list .kv'));
    const started = rows.find((r) => r.querySelector('span')?.textContent === 'Started');
    expect(started).toBeDefined();
    expect(started!.querySelector('b')?.textContent?.trim()).toBe(
      new Date('2026-08-25T10:00:00Z').toLocaleString(),
    );
  });

  // ---- attested deposit: the three-step proof anatomy -----------------

  it('renders the three proof-anatomy step titles in order for an attested deposit', async () => {
    const fixture = setupAttested();
    await tick();
    fixture.detectChanges();
    const titles = Array.from(root(fixture).querySelectorAll('.step-title h4')).map(
      (h) => h.textContent?.trim(),
    );
    expect(titles).toEqual([
      'Deposit seen on Ethereum slot 100',
      'Height attested by the watcher (100)',
      'Deposit confirmed · credits minted',
    ]);
  });

  it('renders the source-block and Solana-mint links as proper external links', async () => {
    const fixture = setupAttested();
    await tick();
    fixture.detectChanges();
    const anchors = Array.from(root(fixture).querySelectorAll('.step a.xlink'));
    const block = anchors.find((a) => a.getAttribute('href') === 'https://ethereum-scan.example.com/block/100');
    const mint = anchors.find((a) => a.getAttribute('href')?.includes('/tx/0xdeadbeef'));
    expect(block).toBeTruthy();
    expect(mint).toBeTruthy();
    for (const a of [block!, mint!]) {
      expect(a.getAttribute('target')).toBe('_blank');
      expect(a.getAttribute('rel')).toBe('noopener');
    }
  });

  it('shows the wire timestamps on the seen and minted steps, none on the attestation step', async () => {
    const fixture = setupAttested();
    await tick();
    fixture.detectChanges();
    const whens = Array.from(root(fixture).querySelectorAll('.step .when')).map(
      (w) => w.textContent?.trim(),
    );
    expect(whens[0]).toBe(new Date('2026-08-25T10:00:00Z').toLocaleString());
    expect(whens[1]).toBe('');
    expect(whens[2]).toBe(new Date('2026-08-25T10:07:00Z').toLocaleString());
  });

  it('renders exactly one step for a direct deposit, with none of the attestation wording', async () => {
    const fixture = setup();
    await tick();
    fixture.detectChanges();
    expect(root(fixture).querySelectorAll('.step').length).toBe(1);
    const text = root(fixture).textContent ?? '';
    expect(text).not.toContain('Deposit seen');
    expect(text).not.toContain('Height attested');
    expect(text).not.toContain('Ethereum');
  });

  // ---- Purchase card: Paid line and pre-mint figures -----------------

  it('renders a direct purchase exactly as today, with no swapped-to line', async () => {
    const fixture = setup();
    await tick();
    fixture.detectChanges();
    const rows = kvRows(fixture);
    expect(rows.get('Paid')!.querySelector('b')?.textContent?.trim()).toBe('250.00 mUSDC');
    // No secondary figure on a direct purchase: the next kv row is Credits.
    const paid = rows.get('Paid')!;
    const next = paid.nextElementSibling as HTMLElement;
    expect(next.querySelector('span')?.textContent).toBe('Credits');
    expect(root(fixture).textContent).not.toContain('swapped to');
  });

  it('shows the identical credits', async () => {
    const fixture = setupPending();
    fakeApi.pending = { ...PENDING_PURCHASE, credits: 250_000, state: 'confirmed', ethBlock: 100 };
    await TestBed.inject(ConsoleStore).startPolling();
    await tick();
    fixture.detectChanges();
    const rows = kvRows(fixture);
    expect(rows.get('Credits')!.querySelector('b')?.textContent?.trim()).toBe('250,000 credits');
    expect(rows.get('Value')!.querySelector('b')?.textContent?.trim()).toBe('$250.00');
    // The settled twin carries the server's minted credits: same figures.
    fakeApi.pending = null;
    fakeApi.attested = true;
    const prevCredits = ATTESTED_PURCHASE.credits;
    ATTESTED_PURCHASE.credits = 250_000;
    await TestBed.inject(ConsoleStore).startPolling();
    await tick();
    fixture.detectChanges();
    const rows2 = kvRows(fixture);
    expect(rows2.get('Credits')!.querySelector('b')?.textContent?.trim()).toBe('250,000 credits');
    expect(rows2.get('Value')!.querySelector('b')?.textContent?.trim()).toBe('$250.00');
    ATTESTED_PURCHASE.credits = prevCredits;
  });
});
