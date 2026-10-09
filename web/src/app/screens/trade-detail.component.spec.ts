import { computed, signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute } from '@angular/router';
import { of } from 'rxjs';
import { ConsoleStore } from '../core/console-store';
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
import { short } from '../core/trace';
import { TradeDetailComponent } from './trade-detail.component';

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
  listingsData: Listing[] = [];
  async nonce(_address: string): Promise<Nonce> {
    return { nonce: 'n', statement: 's', issuedAt: '', expiresAt: '' };
  }
  async verify(_a: string, _n: string, _s: string): Promise<VerifyResult> {
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
    throw new ApiError(500, 'internal', 'no');
  }
  async fundingList(): Promise<FundingStatus[]> {
    return [];
  }
  async listings(_before?: string): Promise<ListingPage> {
    return { rows: this.listingsData.map((l) => ({ ...l })), nextCursor: null };
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

/** A sold listing with every trade fact the detail view renders. */
function soldListing(role: 'buying' | 'selling'): Listing {
  return {
    id: '42',
    listedTx: '0x00000000000000000000000000000000000000listtx',
    state: 'sold',
    role,
    credits: 10_000,
    price: 10_000_000_000,
    token: 'mUSD1',
    tokenAddress: 'DemoUSDCmint11111111111111111111111111111111',
    tokenDecimals: 6,
    matured: true,
    createdAt: '2026-08-15T09:30:00Z',
    listedBlock: 40201,
    maturesAt: 1755100000,
    soldTx: '0x00000000000000000000000000000000000000saletx',
    soldBlock: 40207,
    soldAt: '2026-08-15T11:45:00Z',
  };
}

/** A cancelled listing carrying only the cancel move. */
function cancelledListing(): Listing {
  return {
    id: '77',
    listedTx: '0x00000000000000000000000000000000000000canlist',
    state: 'cancelled',
    role: 'selling',
    credits: 3_000,
    price: 3_000_000_000,
    token: 'mUSD1',
    tokenAddress: 'DemoUSDCmint11111111111111111111111111111111',
    tokenDecimals: 6,
    matured: true,
    createdAt: '2026-08-18T10:00:00Z',
    listedBlock: 40185,
    maturesAt: 1755400000,
    cancelledTx: '0x0000000000000000000000000000000000000canceltx',
    cancelledBlock: 40190,
    cancelledAt: '2026-08-20T08:00:00Z',
  };
}

describe('TradeDetailComponent', () => {
  // The store's stream gate is environment.useMock; keep it true so these
  // specs never open a real EventSource (jsdom has none).
  beforeAll(() => {
    environment.useMock = true;
  });

  let fakeWallet: WalletService;
  let fakeApi: FakeApi;

  function setup(id: string | null) {
    fakeWallet = makeFakeWallet();
    fakeApi = new FakeApi();
    TestBed.configureTestingModule({
      imports: [TradeDetailComponent],
      providers: [
        { provide: SLOTFARE_API, useValue: fakeApi },
        { provide: WalletService, useValue: fakeWallet },
        ConsoleStore,
        {
          provide: ActivatedRoute,
          useValue: {
            paramMap: of({ get: () => id }),
            snapshot: { paramMap: { get: () => id } },
          },
        },
      ],
    });
    const fixture = TestBed.createComponent(TradeDetailComponent);
    fakeWallet.address.set('0xabc');
    fakeWallet.accountId.set('0xmockacct');
    fakeWallet.token.set('mock-token');
    fixture.detectChanges();
    return fixture;
  }

  function root(fixture: ComponentFixture<TradeDetailComponent>): HTMLElement {
    return fixture.nativeElement as unknown as HTMLElement;
  }

  async function show(fixture: ComponentFixture<TradeDetailComponent>): Promise<void> {
    void fixture;
    await TestBed.inject(ConsoleStore).loadListings();
    await tick();
    fixture.detectChanges();
  }

  afterEach(() => TestBed.inject(ConsoleStore).stopPolling());

  it('renders the selling side of a sold trade: proceeds, maturation and sale facts', async () => {
    const fixture = setup('42');
    fakeApi.listingsData = [soldListing('selling')];
    await show(fixture);

    const text = root(fixture).textContent ?? '';
    expect(text).toContain('Proceeds received');
    // The scaled price (raw units / 10^6) next to the pay-token symbol.
    expect(text).toContain('10,000.00 mUSD1');
    // The listing transaction short hash.
    expect(text).toContain('0x00000000…listtx');
    // The maturation deadline derived from the wire's unix seconds,
    // rendered in the runner's local zone by the shared helper.
    expect(text).toContain(new Date(1755100000 * 1000).toLocaleString());
    // The sale block number.
    expect(text).toContain('40,207');

    // Every external explorer link points at the Solana explorer.
    const links = Array.from(root(fixture).querySelectorAll('a.xlink[target="_blank"]'));
    expect(links.length).toBeGreaterThan(0);
    for (const a of links) {
      expect(a.getAttribute('href')).toMatch(
        /^https:\/\/explorer\.solana\.com\/tx\//,
      );
    }
    // The sale link carries the sale hash.
    expect(
      links.some((a) =>
        a.getAttribute('href')!.includes(
          '/tx/0x00000000000000000000000000000000000000saletx',
        ),
      ),
    ).toBe(true);
  });

  it('renders the buying side of the same listing: credits bought, not proceeds', async () => {
    const fixture = setup('42');
    fakeApi.listingsData = [soldListing('buying')];
    await show(fixture);

    const text = root(fixture).textContent ?? '';
    expect(text).toContain('Credits bought: +10,000');
    expect(text).not.toContain('Proceeds received');
    // Same sale-tx link as the selling side.
    const saleLink = Array.from(root(fixture).querySelectorAll('a.xlink[target="_blank"]')).find(
      (a) =>
        a
          .getAttribute('href')!
          .includes('/tx/0x00000000000000000000000000000000000000saletx'),
    );
    expect(saleLink).toBeDefined();
  });

  it('says Bought on the buyer pill for a sold trade', async () => {
    const fixture = setup('42');
    fakeApi.listingsData = [soldListing('buying')];
    await show(fixture);
    const pill = root(fixture).querySelector('.pill');
    expect(pill?.textContent?.trim()).toBe('Bought');
  });

  it('says Sold on the seller pill for the same sold trade', async () => {
    const fixture = setup('42');
    fakeApi.listingsData = [soldListing('selling')];
    await show(fixture);
    const pill = root(fixture).querySelector('.pill');
    expect(pill?.textContent?.trim()).toBe('Sold');
  });

  it('uses purchase-perspective labels on the buyer detail', async () => {
    const fixture = setup('42');
    fakeApi.listingsData = [soldListing('buying')];
    await show(fixture);

    const text = root(fixture).textContent ?? '';
    expect(text).toContain('Purchase transaction');
    expect(text).toContain('Purchase slot');
    expect(text).toContain('Bought at');
    expect(text).not.toContain('Sale transaction');
    expect(text).not.toContain('Sale slot');
  });

  it('renders only the cancel facts for a cancelled listing', async () => {
    const fixture = setup('77');
    fakeApi.listingsData = [cancelledListing()];
    await show(fixture);

    const text = root(fixture).textContent ?? '';
    expect(text).toContain('Cancelled');
    expect(text).toContain('Credits released: 3,000 back to your balance');
    // No sale section for a cancelled row.
    expect(text).not.toContain('Sale transaction');
    // The cancel link carries the cancel hash.
    const cancelLink = Array.from(root(fixture).querySelectorAll('a.xlink[target="_blank"]')).find(
      (a) =>
        a
          .getAttribute('href')!
          .includes('/tx/0x0000000000000000000000000000000000000canceltx'),
    );
    expect(cancelLink).toBeDefined();
  });

  it('shows the empty block when the routed id is not in the account', async () => {
    const fixture = setup('nope');
    fakeApi.listingsData = [soldListing('selling')];
    await show(fixture);

    expect(root(fixture).textContent).toContain('Listing not found');
  });

  it('renders a full 64-char listing hash middle-truncated, never raw', async () => {
    const fixture = setup('42');
    const fullHash = `0x${'a'.repeat(62)}`;
    fakeApi.listingsData = [{ ...soldListing('selling'), listedTx: fullHash }];
    await show(fixture);

    const text = root(fixture).textContent ?? '';
    expect(text).toContain(short(fullHash));
    // The raw hash only lives in the explorer link's href attribute, not in
    // any rendered text node.
    expect(text).not.toContain(fullHash);
  });
});
