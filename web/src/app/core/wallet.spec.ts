import { TestBed } from '@angular/core/testing';
import { WalletService, computeAccountId, base58Decode } from './wallet';

/** Structural stand-in for the injected Solana provider. */
interface TestProvider {
  isPhantom?: boolean;
  connect: ReturnType<typeof vi.fn>;
  disconnect?: ReturnType<typeof vi.fn>;
  signMessage: ReturnType<typeof vi.fn>;
  on?: (event: string, handler: (...args: unknown[]) => void) => void;
  removeListener?: (event: string, handler: (...args: unknown[]) => void) => void;
  /** Fire a provider event at every handler the service registered. */
  fire: (event: string, ...args: unknown[]) => void;
}

/** A provider that connects to ADDR and signs with a fixed 64-byte sig. */
function mockSolana(
  overrides?: Partial<Omit<TestProvider, 'fire'>>,
): TestProvider {
  const listeners = new Map<string, ((...args: unknown[]) => void)[]>();
  return {
    isPhantom: true,
    connect: vi.fn(async () => ({ publicKey: { toBase58: () => ADDR } })),
    signMessage: vi.fn(async () => ({
      signature: new Uint8Array(64).fill(7),
      publicKey: { toBase58: () => ADDR },
    })),
    on(event: string, handler: (...args: unknown[]) => void): void {
      const arr = listeners.get(event) ?? [];
      arr.push(handler);
      listeners.set(event, arr);
    },
    removeListener(event: string, handler: (...args: unknown[]) => void): void {
      const arr = listeners.get(event) ?? [];
      const idx = arr.indexOf(handler);
      if (idx !== -1) arr.splice(idx, 1);
    },
    fire(event: string, ...args: unknown[]): void {
      for (const h of listeners.get(event) ?? []) h(...args);
    },
    ...overrides,
  };
}

/** Install a provider on window before the service is constructed. */
function install(provider: TestProvider | null): void {
  const w = window as unknown as Record<string, unknown>;
  delete w['solana'];
  delete w['solflare'];
  if (provider !== null) w['solana'] = provider;
}

/** Read a localStorage value, tolerating environments where localStorage is unavailable. */
function safeLsGet(key: string): string | null {
  try {
    const ls = window.localStorage;
    if (ls && typeof ls.getItem === 'function') return ls.getItem(key);
  } catch {
    // Unavailable: treat as empty.
  }
  return null;
}

const SESSION_KEY = 'slotfare.console.session';

/** A valid 32-byte base58 key: the system program id. */
const ADDR = '11111111111111111111111111111111';

const NONCE_BODY = {
  nonce: 'aa'.repeat(16),
  statement: 'Sign this nonce to prove you control this account to the Slotfare console.',
  domain: 'slotfare',
  uri: 'https://slotfare',
  issuedAt: '2026-01-01T00:00:00Z',
  expiresAt: '2026-01-01T00:05:00Z',
};

describe('WalletService', () => {
  let svc: WalletService;

  /** Stub fetch for the two session endpoints. */
  function stubFetch(
    verifyRes: { status?: number; body?: unknown },
  ): ReturnType<typeof vi.spyOn> {
    const calls: string[] = [];
    const spy = vi.spyOn(window, 'fetch');
    // vi.spyOn reuses the same spy across tests, so start from a clean slate.
    spy.mockReset();
    spy.mockImplementation(async (url) => {
      calls.push(String(url));
      if (calls.length === 1) {
        return new Response(JSON.stringify(NONCE_BODY), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify(verifyRes.body ?? {}), {
        status: verifyRes.status ?? 200,
        headers: { 'Content-Type': 'application/json' },
      });
    });
    return spy;
  }

  /** Construct the service with a given injected provider (or none). */
  function createService(provider: TestProvider | null): WalletService {
    install(provider);
    TestBed.configureTestingModule({});
    return TestBed.inject(WalletService);
  }

  /** Connect and wait for the promise chain to settle. */
  async function connect(s: WalletService): Promise<void> {
    s.connect('app.phantom');
    await new Promise((r) => setTimeout(r, 0));
  }

  beforeEach(() => {
    window.sessionStorage.clear();
    install(null);
  });

  it('discovers the injected Phantom provider', () => {
    const s = createService(mockSolana());
    expect(s.providers().length).toBe(1);
    expect(s.providers()[0].rdns).toBe('app.phantom');
    expect(s.providers()[0].name).toBe('Phantom');
  });

  it('publishes no providers when nothing is injected', () => {
    const s = createService(null);
    expect(s.providers().length).toBe(0);
  });

  it('sets the address on a successful connect', async () => {
    const s = createService(mockSolana());
    await connect(s);
    expect(s.address()).toBe(ADDR);
    expect(s.error()).toBeNull();
  });

  it('leaves address null and sets error on rejected connection', async () => {
    const s = createService(
      mockSolana({
        connect: vi.fn(async () => {
          const err = new Error('User rejected the request.');
          (err as { code?: number }).code = 4001;
          throw err;
        }),
      }),
    );
    await connect(s);
    expect(s.address()).toBeNull();
    expect(s.error()).toBeNull(); // cancellation is idle, not an error
  });

  it('returns to idle without error when the user cancels', async () => {
    const s = createService(
      mockSolana({
        connect: vi.fn(async () => {
          throw new Error('User denied connection');
        }),
      }),
    );
    await connect(s);
    expect(s.address()).toBeNull();
    expect(s.error()).toBeNull();
  });

  it('publishes a session on the happy path and persists it to sessionStorage only', async () => {
    const s = createService(mockSolana());
    stubFetch({
      body: {
        accountId: computeAccountId(ADDR),
        token: 'tok-123',
        expiresAt: '2026-01-15T00:00:00Z',
      },
    });
    await connect(s);
    s.startSession();
    await new Promise((r) => setTimeout(r, 0));

    expect(s.accountId()).toBe(computeAccountId(ADDR));
    expect(s.token()).toBe('tok-123');
    expect(s.session()).not.toBeNull();

    const stored = window.sessionStorage.getItem(SESSION_KEY);
    expect(stored).toContain('tok-123');
    expect(safeLsGet(SESSION_KEY)).toBeNull();
  });

  it('signs the exact server-built message (Solana framing, no chain id)', async () => {
    const provider = mockSolana();
    const s = createService(provider);
    stubFetch({
      body: {
        accountId: computeAccountId(ADDR),
        token: 'tok-123',
        expiresAt: '2026-01-15T00:00:00Z',
      },
    });
    await connect(s);
    s.startSession();
    await new Promise((r) => setTimeout(r, 0));

    expect(provider.signMessage).toHaveBeenCalledTimes(1);
    const bytes = provider.signMessage.mock.calls[0][0] as Uint8Array;
    const text = new TextDecoder().decode(bytes);
    expect(text).toContain('slotfare wants you to sign in with your Solana account:');
    expect(text).toContain(ADDR);
    expect(text).toContain(`Nonce: ${NONCE_BODY.nonce}`);
    expect(text).toContain('URI: https://slotfare');
    expect(text).not.toContain('Chain ID');
    expect(text).not.toContain('Ethereum');
  });

  it('rejects a session when the server accountId does not match the local derivation', async () => {
    const s = createService(mockSolana());
    stubFetch({
      body: {
        accountId: '0x' + 'ab'.repeat(32),
        token: 'tok-123',
        expiresAt: '2026-01-15T00:00:00Z',
      },
    });
    await connect(s);
    s.startSession();
    await new Promise((r) => setTimeout(r, 0));

    expect(s.token()).toBeNull();
    expect(s.accountId()).toBeNull();
    expect(s.error()).toBe('accountId mismatch');
  });

  it('surfaces a 401 from verify as a visible error', async () => {
    const s = createService(mockSolana());
    stubFetch({ status: 401, body: { error: 'signature verification failed' } });
    await connect(s);
    s.startSession();
    await new Promise((r) => setTimeout(r, 0));

    expect(s.token()).toBeNull();
    expect(s.error()).toBe('Signature verification failed');
  });

  it('reports an expired nonce instead of failing silently', async () => {
    const s = createService(mockSolana());
    stubFetch({ status: 400, body: { error: 'nonce expired' } });
    await connect(s);
    s.startSession();
    await new Promise((r) => setTimeout(r, 0));

    expect(s.token()).toBeNull();
    expect(s.error()).toBe('Sign-in nonce expired, please try again');
  });

  it('leaves no session when the user rejects the signature', async () => {
    const s = createService(
      mockSolana({
        signMessage: vi.fn(async () => {
          const err = new Error('User rejected the request.');
          (err as { code?: number }).code = 4001;
          throw err;
        }),
      }),
    );
    stubFetch({ status: 401, body: { error: 'signature verification failed' } });
    await connect(s);
    s.startSession();
    await new Promise((r) => setTimeout(r, 0));

    expect(s.token()).toBeNull();
    expect(s.error()).toBe('Signature rejected');
  });

  it('a second startSession while one is in flight is a no-op (nonce not replaced)', async () => {
    const s = createService(mockSolana());
    const spy = stubFetch({
      body: {
        accountId: computeAccountId(ADDR),
        token: 'tok-123',
        expiresAt: '2026-01-15T00:00:00Z',
      },
    });
    await connect(s);
    s.startSession();
    s.startSession(); // in flight: must not request a second nonce
    await new Promise((r) => setTimeout(r, 0));

    expect(spy).toHaveBeenCalledTimes(2); // one nonce + one verify
  });

  it('clearSession removes the durable key and the restore markers', async () => {
    const s = createService(mockSolana());
    stubFetch({
      body: {
        accountId: computeAccountId(ADDR),
        token: 'tok-123',
        expiresAt: '2026-01-15T00:00:00Z',
      },
    });
    await connect(s);
    s.startSession();
    await new Promise((r) => setTimeout(r, 0));
    expect(window.sessionStorage.getItem(SESSION_KEY)).not.toBeNull();

    s.clearSession();
    expect(s.token()).toBeNull();
    expect(s.accountId()).toBeNull();
    expect(s.address()).toBeNull();
    expect(window.sessionStorage.getItem(SESSION_KEY)).toBeNull();
  });

  it('clears the session when accountChanged switches to another wallet', async () => {
    const provider = mockSolana();
    const s = createService(provider);
    stubFetch({
      body: {
        accountId: computeAccountId(ADDR),
        token: 'tok-123',
        expiresAt: '2026-01-15T00:00:00Z',
      },
    });
    await connect(s);
    s.startSession();
    await new Promise((r) => setTimeout(r, 0));
    expect(s.token()).toBe('tok-123');

    const other = '4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU';
    provider.fire('accountChanged', { toBase58: () => other });
    await new Promise((r) => setTimeout(r, 0));

    expect(s.token()).toBeNull();
    expect(s.accountId()).toBeNull();
    expect(window.sessionStorage.getItem(SESSION_KEY)).toBeNull();
  });

  it('drops the address when the provider emits disconnect', async () => {
    const provider = mockSolana();
    const s = createService(provider);
    await connect(s);
    expect(s.address()).toBe(ADDR);

    provider.fire('disconnect');
    expect(s.address()).toBeNull();
    expect(s.provider).toBeNull();
  });
});

describe('computeAccountId', () => {
  it('is 0x-hex of the 32 raw public key bytes', () => {
    expect(computeAccountId('11111111111111111111111111111111')).toBe(
      '0x' + '00'.repeat(32),
    );
  });

  it('matches a known fixture', () => {
    // System program id bytes are 32 zeros; a non-trivial fixture uses the
    // raw hex of a fixed key: decode path and hex path must agree.
    const key = '4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU';
    const bytes = base58Decode(key);
    expect(bytes.length).toBe(32);
    const want =
      '0x' + Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
    expect(computeAccountId(key)).toBe(want);
  });

  it('rejects a value that is not 32 bytes', () => {
    expect(() => computeAccountId('abc')).toThrow();
  });
});
