import { InjectionToken, inject } from '@angular/core';
import { environment, Network } from '../../environments/environment';

/** Wire shape of the `config.json` document the container writes at boot. */
export interface RuntimeConfig {
  apiUrl?: string;
  network?: string;
}

/**
 * Base URL of the console API, resolved at boot. The factory default is the
 * compiled `environment.apiUrl`, so every TestBed suite resolves it without
 * an explicit provider; `main.ts` appends a concrete value when the runtime
 * config supplies one.
 */
export const API_BASE_URL = new InjectionToken<string>('API_BASE_URL', {
  factory: () => environment.apiUrl,
});

/**
 * The network the console fronts, resolved at boot the same way as the API
 * base: the compiled `environment.network` unless `config.json` names one.
 */
export const NETWORK = new InjectionToken<Network>('NETWORK', {
  factory: () => environment.network,
});

/**
 * Only the two known network names are accepted from the runtime config;
 * anything else (absent, empty, a typo) keeps the compiled default, so a
 * misconfigured container can never silently hide the testnet disclaimer.
 */
export function resolveNetwork(runtime: RuntimeConfig | null): Network {
  return runtime?.network === 'testnet' || runtime?.network === 'mainnet'
    ? runtime.network
    : environment.network;
}

/**
 * Present wins, absent falls back: a non-empty string from the runtime
 * config replaces the compiled base; anything else (no config, empty or
 * malformed value) keeps `environment.apiUrl`. The single place that rule
 * lives.
 */
export function resolveApiBaseUrl(runtime: RuntimeConfig | null): string {
  return typeof runtime?.apiUrl === 'string' && runtime.apiUrl.length > 0
    ? runtime.apiUrl
    : environment.apiUrl;
}

/**
 * Fetch `config.json` relative to the app's own origin. `cache: 'no-store'`
 * because a stale config would point the app at the wrong API. Any failure
 * (404 in dev, network error, malformed JSON) resolves to `null`; this never
 * throws, so the app boots on the compiled fallback instead of white-screening.
 */
export async function loadRuntimeConfig(): Promise<RuntimeConfig | null> {
  try {
    const res = await fetch('config.json', { cache: 'no-store' });
    if (!res.ok) return null;
    const body: unknown = await res.json();
    return typeof body === 'object' && body !== null && !Array.isArray(body)
      ? (body as RuntimeConfig)
      : null;
  } catch {
    return null;
  }
}

/** Convenience for services that prefer constructor-free injection. */
export function apiBaseUrl(): string {
  return inject(API_BASE_URL);
}
