/** Which Slotfare network this console fronts. Drives cluster defaults such
 *  as the devnet suffix on explorer links. */
export type Network = 'testnet' | 'mainnet';

export interface Environment {
  /** Base URL of the console API; `/console/...` paths are appended to it. */
  apiUrl: string;
  /** The network this build targets. The container's `NETWORK` env overrides
   *  it at boot through `config.json`. */
  network: Network;
  /** T03 switches on mock API selection with this flag. */
  useMock: boolean;
  /** `domain` claim in the sign-in message (mirrors server defaults). */
  sessionDomain: string;
  /** `uri` claim in the sign-in message (mirrors server defaults). */
  sessionUri: string;
}

/**
 * The compiled console environment. Chain facts - mint, treasury, RPC,
 * explorer - are deliberately absent: they are served at runtime by
 * `GET /console/deposit-target` and `GET /console/chains`, so a build never
 * hardcodes a deployment.
 */
export const environment: Environment = {
  // Relative on purpose: slotfared sends no CORS headers, so the browser must
  // reach it through the dev-server proxy on the console's own origin
  // (proxy.conf.json forwards /console and /rpc to slotfared).
  apiUrl: '',
  // Solana devnet is the default deployment target.
  network: 'testnet',
  // The real slotfared through the dev proxy. Set true for the offline demo.
  useMock: false,
  sessionDomain: 'slotfare',
  sessionUri: 'https://slotfare',
};
