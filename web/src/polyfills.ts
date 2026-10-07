/**
 * Browser polyfills, loaded ahead of the application bundle.
 *
 * `@solana/spl-token` and its BigInt helpers build bytes with the Node
 * `Buffer` global at module scope. Under the dev server Vite externalizes
 * Node built-ins, so that global is absent and the module throws
 * `ReferenceError: Buffer is not defined` while the dependency graph is
 * still evaluating - which takes the whole console down with it, since the
 * bootstrap never runs. Defining it here, before anything imports the
 * Solana packages, is what lets the console boot outside Node.
 *
 * `buffer` is a direct dependency for that reason (the transitive copy the
 * Solana packages carry is not something this file should rely on).
 */
import { Buffer } from 'buffer';

type PolyfillTarget = typeof globalThis & {
  Buffer?: typeof Buffer;
  global?: typeof globalThis;
};

const target = globalThis as PolyfillTarget;
target.Buffer ??= Buffer;
target.global ??= globalThis;
