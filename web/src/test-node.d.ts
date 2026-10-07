/**
 * Test-only ambient declaration: the unit-test runner executes in Node, so
 * node built-ins are importable from specs even though the app build has no
 * @types/node. Kept minimal on purpose.
 */
declare module 'node:fs' {
  export function readFileSync(path: string, encoding: string): string;
  export function existsSync(path: string): boolean;
  export interface Dirent {
    name: string;
    isDirectory(): boolean;
  }
  export function readdirSync(
    path: string,
    options?: { withFileTypes?: boolean },
  ): Dirent[];
}
declare module 'node:path' {
  export function join(...parts: string[]): string;
}
declare const process: { cwd(): string };
