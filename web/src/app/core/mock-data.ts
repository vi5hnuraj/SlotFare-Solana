import { ApiKey, StableToken, UsageRow, Wallet } from './models';
import { merkleRoot, usageLeafHash } from './commitment';

/**
 * Fixtures for the mock. Everything here is display-only and gets replaced
 * by the metering API; the shapes are what the real endpoints must return.
 */

export const WALLETS: Wallet[] = [
  { name: 'MetaMask', rdns: 'io.metamask', initial: 'M' },
  { name: 'Rabby', rdns: 'io.rabby', initial: 'R' },
  { name: 'Coinbase Wallet', rdns: 'com.coinbase.wallet', initial: 'C' },
  { name: 'Frame', rdns: 'sh.frame', initial: 'F' },
];

export const TOKENS: StableToken[] = [
  { symbol: 'USDC', held: '1,842.60', available: true },
  { symbol: 'USDT', held: '96.00', available: true },
  { symbol: 'DAI', held: '310.44', available: true },
  { symbol: 'PYUSD', held: 'none held', available: false },
];

export const PRESET_AMOUNTS = [25, 100, 250, 500];

export const OPENING_BALANCE = 412_860;
/** Whole credits the demo account holds in open sell listings; the mock
 *  account's `locked` figure so the locked card is visible in the demo. */
export const OPENING_LOCKED = 5_000;
export const WALLET_ADDRESS = '0x7a3F…9C21';

export const KEYS: ApiKey[] = [
  {
    id: '7Qk4',
    display: 'slf_live_7Qk4…3f2a',
    label: 'prod-router',
    created: '2026-06-14',
    lastUsedAt: '2026-08-22T19:58:00Z',
    requests: 184_203,
    revoked: false,
  },
  {
    id: '2Mv9',
    display: 'slf_live_2Mv9…c803',
    label: 'batch-eval',
    created: '2026-07-02',
    lastUsedAt: '2026-08-22T16:00:00Z',
    requests: 12_880,
    revoked: false,
  },
  {
    id: '9Ht1',
    display: 'slf_live_9Ht1…ab77',
    label: 'staging',
    created: '2026-08-09',
    lastUsedAt: null,
    requests: 0,
    revoked: false,
  },
  {
    id: '4Rz2',
    display: 'slf_live_4Rz2…10de',
    label: 'laptop-cli',
    created: '2026-05-30',
    lastUsedAt: '2026-08-01T09:00:00Z',
    requests: 3_412,
    revoked: true,
  },
];

const M = 'qwen3-8-27b';

/** The raw fixture rows, before the settlement facts are attached. */
const RAW_USAGE: Omit<UsageRow, 'commitment'>[] = [
  { id: 'u0', time: '14:22:07', key: '7Qk4', model: M, prompt: 8_412, cached: 6_144, completion: 1_206, credits: 12.6444, result: 'ok', latencyMs: 412, error: null },
  { id: 'u1', time: '14:21:54', key: '7Qk4', model: M, prompt: 2_190, cached: 0, completion: 884, credits: 4.842, result: 'ok', latencyMs: 391, error: null },
  { id: 'u2', time: '14:21:31', key: '2Mv9', model: M, prompt: 41_880, cached: 32_768, completion: 2_412, credits: 52.3928, result: 'ok', latencyMs: 905, error: null },
  { id: 'u3', time: '14:20:48', key: '7Qk4', model: M, prompt: 1_104, cached: 896, completion: 318, credits: 2.1476, result: 'ok', latencyMs: 214, error: null },
  { id: 'u4', time: '14:20:12', key: '2Mv9', model: M, prompt: null, cached: null, completion: null, credits: 0, result: 'error', latencyMs: null, error: '400 context_length' },
  { id: 'u5', time: '14:19:02', key: '7Qk4', model: M, prompt: 15_620, cached: 12_288, completion: 4_096, credits: 29.1368, result: 'ok', latencyMs: 1_288, error: null },
  { id: 'u6', time: '14:17:40', key: '2Mv9', model: M, prompt: 98_304, cached: 81_920, completion: 1_024, credits: 109.568, result: 'ok', latencyMs: 604, error: null },
  { id: 'u7', time: '14:16:12', key: '7Qk4', model: M, prompt: 3_308, cached: 0, completion: 1_512, credits: 7.844, result: 'ok', latencyMs: 498, error: null },
  { id: 'u8', time: '14:15:37', key: '7Qk4', model: M, prompt: 512, cached: 0, completion: 96, credits: 0.8, result: 'ok', latencyMs: 132, error: null },
  { id: 'u9', time: '14:15:04', key: '7Qk4', model: M, prompt: 200, cached: 0, completion: 50, credits: 0.0005, result: 'ok', latencyMs: 98, error: null },
];

/** The demo's settlement facts: one batch holds every settled row. The
 *  newest ok row is still awaiting settlement, and the error row was never
 *  charged, so neither carries a commitment. The root is computed with the
 *  same scheme the mock's proof tree and the client-side verifier use, so
 *  demo, mock proof and test all agree on one value. */
const PENDING_ROW_ID = 'u0';
const MOCK_BATCH_INDEX = 12;
const MOCK_COMMIT_TX =
  '0x8f3a2c7d9e1b46a50c7d2f8e3a1b96c45d0e7f8a2b3c4d5e6f708192a3b4c5d6';

const settledLeaves = RAW_USAGE.filter(
  (r) => r.result === 'ok' && r.id !== PENDING_ROW_ID,
).map((r) =>
  usageLeafHash({
    occurredAt: r.time,
    keyId: r.key,
    model: r.model,
    promptTokens: r.prompt,
    cachedTokens: r.cached,
    completionTokens: r.completion,
    credits: Math.round(r.credits * 10_000),
  }),
);
const SETTLED_ROOT = merkleRoot(settledLeaves);

export const USAGE: UsageRow[] = RAW_USAGE.map((r) =>
  r.result === 'ok' && r.id !== PENDING_ROW_ID
    ? {
        ...r,
        commitment: {
          batchIndex: MOCK_BATCH_INDEX,
          root: SETTLED_ROOT,
          commitTx: MOCK_COMMIT_TX,
        },
      }
    : { ...r, commitment: null },
);

/** 2026-07-24 through 2026-08-22. Weekends dip, the trend rises, the last
 *  day is partial because it is today. */
export const DAILY_SPEND = [
  22_470, 9_840, 7_620, 24_980, 26_110, 25_630, 21_050,
  27_640, 12_080, 8_930, 28_910, 31_200, 29_450, 30_120,
  33_480, 14_260, 10_510, 31_970, 29_880, 32_040, 35_110,
  34_290, 15_980, 11_740, 36_720, 34_880, 33_160, 31_840,
  30_460, 13_420,
];

/** Epoch millis for the first bar. A constant, not `new Date()`, so the
 *  chart renders identically in a test and in a screenshot. */
export const DAY_ZERO = Date.UTC(2026, 6, 24);

/** Requests per day over the same 30-day window as DAILY_SPEND, with the
 *  same weekday/weekend shape (weekends dip). Demo data served by
 *  MockSlotfareApi; it is the fixture source for the usage screen's request
 *  count and per-request mean. */
export const DAILY_REQUESTS = [
  910, 402, 305, 980, 1_030, 1_005, 830,
  1_090, 480, 355, 1_140, 1_230, 1_160, 1_190,
  1_320, 560, 410, 1_260, 1_180, 1_270, 1_390,
  1_350, 630, 460, 1_450, 1_380, 1_310, 1_250,
  1_200, 530,
];
