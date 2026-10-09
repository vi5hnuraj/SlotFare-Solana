import { signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { SellService, classifySellError, SELL_ERRORS } from './sell';
import { ApiError, SlotfareApi, SLOTFARE_API } from './api';
import { Listing } from './models';
import { WalletService } from './wallet';
import { environment } from '../../environments/environment';

const ADDR = '7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU';

function row(overrides: Partial<Listing> = {}): Listing {
  return {
    id: '7',
    listedTx: '5listtx',
    state: 'open',
    role: 'selling',
    credits: 5_000,
    price: 500_000_000,
    token: 'USDC',
    tokenAddress: 'DemoUSDCmint11111111111111111111111111111111',
    tokenDecimals: 6,
    matured: true,
    createdAt: '2026-08-22T20:00:00Z',
    ...overrides,
  };
}

function makeApi(overrides: Partial<Record<string, unknown>> = {}) {
  const calls = {
    createListing: [] as { credits: number; price: number }[],
    cancelListing: [] as string[],
    prepareMarket: [] as { action: string; listingId?: string; credits?: number; price?: number }[],
    depositTarget: 0,
  };
  const api = {
    createListing: async (body: { credits: number; price: number }) => {
      calls.createListing.push(body);
      return row();
    },
    cancelListing: async (id: string) => {
      calls.cancelListing.push(id);
    },
    // A prepared list/cancel: the nonce and the (deliberately unusable)
    // wire bytes the prepare route would hand the wallet.
    prepareMarket: async (body: {
      action: string;
      listingId?: string;
      credits?: number;
      price?: number;
    }) => {
      calls.prepareMarket.push(body);
      return { action: body.action, nonce: '1', transaction: 'not-a-wire-transaction' };
    },
    confirmMarket: async () => row(),
    depositTarget: async () => {
      calls.depositTarget += 1;
      return {
        treasury: 'DemoTreasuryTok1111111111111111111111111111111',
        mint: 'DemoUSDCmint11111111111111111111111111111111',
        cluster: 'devnet',
        rpc: 'https://api.devnet.solana.com',
        explorer: 'https://explorer.solana.com',
        decimals: 6,
        creditsPerUsd: 1000,
        microCreditsPerUsdBase: 10,
      };
    },
    ...overrides,
  };
  return { api: api as unknown as SlotfareApi, calls };
}

function makeWallet(address: string | null = ADDR) {
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
  return TestBed.inject(SellService);
}

describe('classifySellError', () => {
  it('produces a pairwise-distinct message per class', () => {
    // One representative per class: the messages must not collapse into
    // each other, whatever input reaches them.
    const messages = [
      classifySellError(new Error('User rejected the request.')),
      classifySellError(new Error('no connected wallet')),
      classifySellError(new Error('insufficient credits for this listing')),
      classifySellError(new Error('not your listing')),
      classifySellError(new Error('listing not open - already moved')),
      classifySellError(new Error('listing not found')),
      classifySellError(new Error('NotMatured(7, 100)')),
      classifySellError(new ApiError(0, 'offline', 'console unreachable')),
      classifySellError(new ApiError(501, 'not_implemented', '')),
      classifySellError('plain server detail'),
      classifySellError(new Error('')),
    ];
    expect(new Set(messages).size).toBe(messages.length);
  });

  it('names each class specifically', () => {
    expect(classifySellError(new Error('User rejected'))).toBe(SELL_ERRORS.rejected);
    expect(classifySellError(new Error('cancelled by user'))).toBe(SELL_ERRORS.rejected);
    expect(classifySellError(new Error('no connected wallet'))).toBe(SELL_ERRORS.noWallet);
    expect(classifySellError(new Error('insufficient credits'))).toBe(SELL_ERRORS.insufficient);
    expect(classifySellError(new Error('not your listing'))).toBe(SELL_ERRORS.forbidden);
    expect(classifySellError(new Error('no longer open'))).toBe(SELL_ERRORS.gone);
    expect(classifySellError(new Error('listing not found'))).toBe(SELL_ERRORS.notFound);
    expect(classifySellError(new Error('not matured yet'))).toBe(
      'This listing has not matured yet; a sale lands after the maturation window.',
    );
    expect(classifySellError(new ApiError(402, 'payment_required', ''))).toBe(
      SELL_ERRORS.insufficient,
    );
    expect(classifySellError(new ApiError(403, 'forbidden', ''))).toBe(SELL_ERRORS.forbidden);
    expect(classifySellError(new ApiError(404, 'not_found', ''))).toBe(SELL_ERRORS.notFound);
    expect(classifySellError(new ApiError(409, 'conflict', ''))).toBe(SELL_ERRORS.gone);
    expect(classifySellError(new ApiError(501, 'not_implemented', ''))).toBe(
      'The market is not available on this deployment.',
    );
    expect(classifySellError(new Error(''))).toBe(SELL_ERRORS.network);
  });

  it('keeps the transport detail visible on a network failure', () => {
    const msg = classifySellError(new ApiError(0, 'offline', 'fetch failed'));
    expect(msg.startsWith(SELL_ERRORS.network)).toBe(true);
    expect(msg).toContain('fetch failed');
  });

  it('keeps an unknown server reason visible instead of hiding it', () => {
    expect(classifySellError(new Error('weird upstream failure'))).toBe(
      'weird upstream failure',
    );
    // The error code joins the message when the transport carries one.
    expect(classifySellError(new Error('denied'))).toBe(SELL_ERRORS.rejected);
  });
});

describe('SellService', () => {
  // The mirror-path tests below are the offline demo's, which is the one
  // deployment where the console is the writer. The escrow path gets its own
  // tests, and the round trip it needs is proven end to end in Go.
  const useMockBefore = environment.useMock;
  beforeEach(() => {
    environment.useMock = true;
  });
  afterEach(() => {
    environment.useMock = useMockBefore;
  });

  it('scales the human price to USDC base units and serves the created row', async () => {
    const { api, calls } = makeApi();
    const svc = await setup(api);

    const created = await svc.list(5_000, 'USDC', 5);

    expect(calls.createListing).toEqual([{ credits: 5_000, price: 5_000_000 }]);
    expect(created.id).toBe('7');
    expect(svc.listingId()).toBe('7');
    expect(svc.step()).toBe('done');
    expect(svc.busy()).toBe(false);
    expect(svc.error()).toBeNull();
  });

  it('rounds the price to whole cents rather than truncating', async () => {
    const { api, calls } = makeApi();
    const svc = await setup(api);

    await svc.list(100, 'USDC', 1.005);
    expect(calls.createListing[0].price).toBe(1_005_000);
  });

  it('never opens a listing without a token symbol', async () => {
    const { api, calls } = makeApi();
    const svc = await setup(api);

    await expect(svc.list(5_000, '', 5)).rejects.toThrow(SELL_ERRORS.rejected);
    expect(svc.error()).toBe(SELL_ERRORS.rejected);
    expect(calls.createListing).toEqual([]);
    expect(svc.step()).toBe('idle');
  });

  it('classifies a rejected listing and rethrows the fixed message', async () => {
    const { api } = makeApi({
      createListing: async () => {
        throw new ApiError(402, 'payment_required', '');
      },
    });
    const svc = await setup(api);

    await expect(svc.list(5_000, 'USDC', 5)).rejects.toThrow(SELL_ERRORS.insufficient);
    expect(svc.error()).toBe(SELL_ERRORS.insufficient);
    expect(svc.step()).toBe('idle');
    expect(svc.listingId()).toBeNull();
  });

  it('cancels an open listing and returns to done', async () => {
    const { api, calls } = makeApi();
    const svc = await setup(api);

    await svc.cancel('7');

    expect(calls.cancelListing).toEqual(['7']);
    expect(svc.step()).toBe('done');
    expect(svc.busy()).toBe(false);
    expect(svc.error()).toBeNull();
  });

  it('classifies a failed cancel and rethrows the fixed message', async () => {
    const { api } = makeApi({
      cancelListing: async () => {
        throw new ApiError(409, 'conflict', '');
      },
    });
    const svc = await setup(api);

    await expect(svc.cancel('7')).rejects.toThrow(SELL_ERRORS.gone);
    expect(svc.error()).toBe(SELL_ERRORS.gone);
    expect(svc.step()).toBe('idle');
  });

  it('rejects a second write while one is in flight', async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const { api } = makeApi({
      createListing: async () => {
        await gate;
        return row();
      },
    });
    const svc = await setup(api);

    const first = svc.list(5_000, 'USDC', 5);
    expect(svc.busy()).toBe(true);
    await expect(svc.list(5_000, 'USDC', 5)).rejects.toThrow(
      'Please wait for the pending request to finish.',
    );

    release();
    await first;
    expect(svc.busy()).toBe(false);
  });

  it('asks the console for a list instruction the wallet can sign', async () => {
    environment.useMock = false;
    const { api, calls } = makeApi();
    const svc = await setup(api);

    // The stub hands back a string that is not a wire transaction, so the
    // round trip stops at the wallet step. What is asserted is the contract:
    // the service asks the console to build the escrow first, reaches for the
    // cluster next, and never falls back to the local mirror.
    await expect(svc.list(5_000, 'USDC', 5)).rejects.toThrow();

    expect(calls.prepareMarket).toEqual([{ action: 'list', credits: 5_000, price: 5_000_000 }]);
    expect(calls.depositTarget).toBe(1);
    expect(calls.createListing).toEqual([]);
    expect(svc.step()).toBe('idle');
  });

  it('withdraws through the escrow rather than the local mirror', async () => {
    environment.useMock = false;
    const { api, calls } = makeApi();
    const svc = await setup(api);

    await expect(svc.cancel('7')).rejects.toThrow();

    expect(calls.prepareMarket).toEqual([{ action: 'cancel', listingId: '7' }]);
    expect(calls.cancelListing).toEqual([]);
  });

  it('refuses to open the escrow without a connected wallet', async () => {
    environment.useMock = false;
    const { api, calls } = makeApi();
    const svc = await setup(api, null);

    await expect(svc.list(5_000, 'USDC', 5)).rejects.toThrow(SELL_ERRORS.noWallet);
    expect(calls.depositTarget).toBe(0);
  });

  it('dismissError clears the message, the step and the last id', async () => {
    const { api } = makeApi();
    const svc = await setup(api);

    await svc.list(5_000, 'USDC', 5);
    expect(svc.listingId()).toBe('7');

    svc.dismissError();
    expect(svc.error()).toBeNull();
    expect(svc.step()).toBe('idle');
    expect(svc.listingId()).toBeNull();
  });
});
