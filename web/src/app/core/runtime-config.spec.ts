import { TestBed } from '@angular/core/testing';
import { provideHttpClient, withFetch } from '@angular/common/http';
import {
  HttpTestingController,
  provideHttpClientTesting,
} from '@angular/common/http/testing';
import { signal } from '@angular/core';
import { WalletService } from './wallet';
import { HttpSlotfareApi } from './api';
import {
  API_BASE_URL,
  loadRuntimeConfig,
  resolveApiBaseUrl,
  resolveNetwork,
} from './runtime-config';
import { environment } from '../../environments/environment';

describe('resolveNetwork', () => {
  it('accepts the two known network names from the runtime config', () => {
    expect(resolveNetwork({ network: 'mainnet' })).toBe('mainnet');
    expect(resolveNetwork({ network: 'testnet' })).toBe('testnet');
  });

  it('keeps the compiled network for anything else', () => {
    expect(resolveNetwork(null)).toBe(environment.network);
    expect(resolveNetwork({})).toBe(environment.network);
    expect(resolveNetwork({ network: '' })).toBe(environment.network);
    expect(resolveNetwork({ network: 'Mainnet' })).toBe(environment.network);
  });
});

describe('resolveApiBaseUrl', () => {
  it('uses the runtime URL when present', () => {
    expect(
      resolveApiBaseUrl({ apiUrl: 'https://slotfare.example.com' }),
    ).toBe('https://slotfare.example.com');
  });

  it('falls back to the compiled base when the config is absent', () => {
    expect(resolveApiBaseUrl(null)).toBe(environment.apiUrl);
  });

  it('falls back when apiUrl is empty, missing or not a string', () => {
    expect(resolveApiBaseUrl({})).toBe(environment.apiUrl);
    expect(resolveApiBaseUrl({ apiUrl: '' })).toBe(environment.apiUrl);
    expect(resolveApiBaseUrl({ apiUrl: 42 as unknown as string })).toBe(
      environment.apiUrl,
    );
  });
});

describe('loadRuntimeConfig', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('resolves the parsed object on a 200 with a JSON body', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ apiUrl: 'https://slotfare.example.com' }),
    });
    vi.stubGlobal('fetch', fetchMock);
    await expect(loadRuntimeConfig()).resolves.toEqual({
      apiUrl: 'https://slotfare.example.com',
    });
    expect(fetchMock).toHaveBeenCalledWith('config.json', { cache: 'no-store' });
  });

  it('resolves null on a 404 (dev server has no config.json)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: false, status: 404 }),
    );
    await expect(loadRuntimeConfig()).resolves.toBeNull();
  });

  it('resolves null when the fetch rejects', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockRejectedValue(new Error('network down')),
    );
    await expect(loadRuntimeConfig()).resolves.toBeNull();
  });

  it('resolves null for a non-object body', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: true, json: async () => 'oops' }),
    );
    await expect(loadRuntimeConfig()).resolves.toBeNull();
  });
});

/** A minimal WalletService stub: token and address only. */
function makeWalletStub() {
  return {
    token: signal<string | null>(null),
    address: signal<string | null>('0x0000000000000000000000000000000000000001'),
  };
}

describe('HttpSlotfareApi with a runtime base URL', () => {
  let http: HttpTestingController;

  function configure(providers: unknown[]): void {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(withFetch()),
        provideHttpClientTesting(),
        { provide: WalletService, useValue: makeWalletStub() },
        HttpSlotfareApi,
        ...providers,
      ],
    });
    http = TestBed.inject(HttpTestingController);
  }

  afterEach(() => http.verify());

  it('sends requests to the runtime URL when the token is overridden', async () => {
    configure([
      {
        provide: API_BASE_URL,
        useValue: 'https://slotfare.example.com',
      },
    ]);
    const api = TestBed.inject(HttpSlotfareApi);
    const p = api.account();
    const req = http.expectOne({
      method: 'GET',
      url: 'https://slotfare.example.com/console/account',
    });
    req.flush(
      {
        accountId: '0xmock1',
        mintedMicro: 0,
        settledMicro: 0,
        pendingMicro: 0,
        lockedMicro: 0,
        spendableMicro: 0,
      },
      { status: 200, statusText: 'OK' },
    );
    await p;
  });

  it('falls back to relative URLs when no override is present', async () => {
    configure([]);
    const api = TestBed.inject(HttpSlotfareApi);
    const p = api.account();
    // The factory default is environment.apiUrl (''), so the request is
    // same-origin relative - exactly today's dev behavior.
    const req = http.expectOne({ method: 'GET', url: '/console/account' });
    req.flush(
      {
        accountId: '0xmock1',
        mintedMicro: 0,
        settledMicro: 0,
        pendingMicro: 0,
        lockedMicro: 0,
        spendableMicro: 0,
      },
      { status: 200, statusText: 'OK' },
    );
    await p;
  });
});
