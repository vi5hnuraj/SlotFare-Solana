import { signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { BuyService, classifyBuyError, BUY_ERRORS } from './buy';
import { ApiError, SlotfareApi, SLOTFARE_API } from './api';
import { MarketListing } from './models';
import { WalletService } from './wallet';
import { TransferError } from './solana';
import { environment } from '../../environments/environment';

const ADDR = '7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU';
const SELLER = '9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM';
const SIG = '5j7s6NiJS3JAkvgkoc18WVAsiSaci2pxB2A6ueCJP4tprA2TFg9wSyTLeYouxPBJEMzJinENTkpA52YStRW5Dia7';

function listing(overrides: Partial<MarketListing> = {}): MarketListing {
  return {
    id: '12',
    credits: 5_000,
    price: 5_000_000,
    token: 'USDC',
    maturesAt: 1756000000,
    matured: true,
    mine: false,
    seller: SELLER,
    ...overrides,
  };
}

/** BuyService reads these two collaborators and nothing else. */
function makeApi(overrides: Partial<Record<string, unknown>> = {}) {
  const calls = {
    depositTarget: 0,
    payListing: [] as string[],
    prepareMarket: [] as { action: string; listingId?: string }[],
  };
  const api = {
    depositTarget: async () => {
      calls.depositTarget += 1;
      return {
        rpc: 'https://api.devnet.solana.com',
        mint: 'DemoUSDCmint11111111111111111111111111111111',
        treasury: 'DemoTreasuryTok1111111111111111111111111111111',
        decimals: 6,
        cluster: 'devnet',
      };
    },
    // A prepared buy: the nonce and the (deliberately unusable) wire bytes
    // the prepare route would hand the wallet.
    prepareMarket: async (body: { action: string; listingId?: string }) => {
      calls.prepareMarket.push(body);
      return { action: body.action, nonce: '1', transaction: 'not-a-wire-transaction' };
    },
    confirmMarket: async () => listing(),
    payListing: async (id: string, signature: string) => {
      calls.payListing.push(`${id}:${signature}`);
      return listing();
    },
    ...overrides,
  };
  return { api: api as unknown as SlotfareApi, calls };
}

function makeWallet(address: string | null) {
  return {
    address: signal<string | null>(address),
    signSolanaTransaction: async (tx: unknown): Promise<unknown> => tx,
  };
}

async function setup(api: SlotfareApi, address: string | null = ADDR) {
  TestBed.configureTestingModule({
    providers: [
      { provide: SLOTFARE_API, useValue: api },
      { provide: WalletService, useValue: makeWallet(address) },
    ],
  });
  return TestBed.inject(BuyService);
}

describe('classifyBuyError', () => {
  it('produces a pairwise-distinct message per class', () => {
    // One representative per class: the messages must not collapse into
    // each other, whatever input reaches them.
    const messages = [
      classifyBuyError(new TransferError('The request was cancelled. Nothing was sent.', { cancelled: true })),
      classifyBuyError(new TransferError('Your wallet does not hold enough USDC for this amount.')),
      classifyBuyError(new Error('no connected wallet')),
      classifyBuyError(new Error('no seller wallet on this listing')),
      classifyBuyError(new Error('execution reverted: NotMatured(7, 100)')),
      classifyBuyError(new Error('listing not open - already moved')),
      classifyBuyError(new ApiError(0, 'offline', 'console unreachable')),
      classifyBuyError(new ApiError(501, 'not_implemented', '')),
      classifyBuyError('plain server detail'),
      classifyBuyError(new Error('')),
    ];
    expect(new Set(messages).size).toBe(messages.length);
  });

  it('names each class specifically', () => {
    expect(classifyBuyError(new TransferError('x', { cancelled: true }))).toBe(BUY_ERRORS.rejected);
    expect(classifyBuyError(new Error('User rejected'))).toBe(BUY_ERRORS.rejected);
    expect(classifyBuyError(new Error('denied by user'))).toBe(BUY_ERRORS.rejected);
    expect(classifyBuyError(new TransferError('boom'))).toBe(BUY_ERRORS.insufficient);
    expect(classifyBuyError(new Error('insufficient funds for transfer'))).toBe(
      BUY_ERRORS.insufficient,
    );
    expect(classifyBuyError(new Error('attempt to debit an account'))).toBe(
      BUY_ERRORS.insufficient,
    );
    expect(classifyBuyError(new Error('no connected wallet'))).toBe(BUY_ERRORS.noWallet);
    expect(classifyBuyError(new Error('no seller wallet'))).toBe(BUY_ERRORS.noSeller);
    expect(classifyBuyError(new Error('execution reverted: NotMatured(7, 100)'))).toBe(
      BUY_ERRORS.notMature,
    );
    expect(classifyBuyError(new Error('already moved'))).toBe(BUY_ERRORS.boughtByAnother);
    expect(classifyBuyError(new ApiError(409, 'conflict', ''))).toBe(BUY_ERRORS.boughtByAnother);
    expect(classifyBuyError(new ApiError(404, 'not_found', ''))).toBe(BUY_ERRORS.boughtByAnother);
    expect(classifyBuyError(new ApiError(501, 'not_implemented', ''))).toBe(
      'The market is not available on this deployment.',
    );
    expect(classifyBuyError(new Error(''))).toBe(BUY_ERRORS.network);
  });

  it('keeps the transport detail visible on a network failure', () => {
    expect(classifyBuyError(new ApiError(0, 'offline', 'fetch failed'))).toBe(
      `${BUY_ERRORS.network} (fetch failed)`,
    );
  });

  it('lets a specific server reason win over the generic classes', () => {
    // A 409 that says "not matured" is an immaturity, not a lost race.
    expect(classifyBuyError(new ApiError(409, 'conflict', 'not matured yet'))).toBe(
      BUY_ERRORS.notMature,
    );
    // An unknown error passes its own text through rather than hiding it.
    expect(classifyBuyError(new Error('weird upstream failure'))).toBe(
      'weird upstream failure',
    );
  });
});

describe('BuyService', () => {
  const useMockBefore = environment.useMock;

  afterEach(() => {
    environment.useMock = useMockBefore;
  });

  it('settles against a stand-in signature in the offline demo', async () => {
    environment.useMock = true;
    const { api, calls } = makeApi();
    const svc = await setup(api);

    const sig = await svc.buy(listing());

    expect(sig).toContain('MockBuySig12');
    expect(calls.payListing).toEqual([`12:${sig}`]);
    expect(svc.step()).toBe('done');
    expect(svc.error()).toBeNull();
    expect(svc.busy()).toBe(false);
  });

  it('refuses to send without a connected wallet', async () => {
    environment.useMock = false;
    const { api, calls } = makeApi();
    const svc = await setup(api, null);

    await expect(svc.buy(listing())).rejects.toThrow(BUY_ERRORS.noWallet);
    expect(svc.error()).toBe(BUY_ERRORS.noWallet);
    expect(svc.step()).toBe('idle');
    expect(calls.depositTarget).toBe(0);
    expect(calls.payListing).toEqual([]);
  });

  it('refuses a listing that carries no seller wallet', async () => {
    environment.useMock = false;
    const { api, calls } = makeApi();
    const svc = await setup(api);

    await expect(svc.buy(listing({ seller: '' }))).rejects.toThrow(BUY_ERRORS.noSeller);
    expect(svc.error()).toBe(BUY_ERRORS.noSeller);
    expect(calls.depositTarget).toBe(0);
    expect(calls.payListing).toEqual([]);
  });

  it('surfaces a lost race as a plain notice, never an error card state alone', async () => {
    environment.useMock = true;
    const { api } = makeApi({
      payListing: async () => {
        throw new ApiError(409, 'conflict', 'listing not open - already moved');
      },
    });
    const svc = await setup(api);

    await expect(svc.buy(listing())).rejects.toThrow(BUY_ERRORS.boughtByAnother);
    expect(svc.listingGone()).toBe(true);
    expect(svc.error()).toBe(BUY_ERRORS.boughtByAnother);

    svc.dismissError();
    expect(svc.listingGone()).toBe(false);
    expect(svc.error()).toBeNull();
    expect(svc.step()).toBe('idle');
  });

  it('does not flag a 409 immaturity as a lost race', async () => {
    environment.useMock = true;
    const { api } = makeApi({
      payListing: async () => {
        throw new ApiError(409, 'conflict', 'not matured yet');
      },
    });
    const svc = await setup(api);

    await expect(svc.buy(listing())).rejects.toThrow(BUY_ERRORS.notMature);
    expect(svc.listingGone()).toBe(false);
    expect(svc.error()).toBe(BUY_ERRORS.notMature);
  });

  it('asks the console for a buyer-signed transaction before it sends anything', async () => {
    environment.useMock = false;
    const { api, calls } = makeApi();
    const svc = await setup(api);

    // The stub hands back a string that is not a wire transaction, so the
    // round trip stops at the wallet step. What is asserted is the contract:
    // the console builds the escrow's Buy first, the browser reaches for the
    // cluster, and no separate USDC transfer is attempted.
    await expect(svc.buy(listing())).rejects.toThrow();

    expect(calls.prepareMarket).toEqual([{ action: 'buy', listingId: '12' }]);
    expect(calls.depositTarget).toBe(1);
    expect(calls.payListing).toEqual([]);
    expect(svc.step()).toBe('idle');
  });

  it('rejects a second prompt while one is in flight', async () => {
    environment.useMock = false;
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const { api } = makeApi({
      prepareMarket: async () => {
        await gate;
        throw new Error('unused');
      },
    });
    const svc = await setup(api);

    const first = svc.buy(listing());
    first.catch(() => {});
    expect(svc.busy()).toBe(true);
    await expect(svc.buy(listing())).rejects.toThrow(
      'Please wait for the pending payment to finish.',
    );

    release();
    await expect(first).rejects.toThrow();
    expect(svc.busy()).toBe(false);
    expect(svc.step()).toBe('idle');
  });
});
