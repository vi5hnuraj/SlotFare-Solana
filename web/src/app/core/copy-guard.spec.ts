// Copy guard: no em dash (U+2014) or en dash (U+2013) anywhere in a
// user-facing string. The console's words pass the same character policy
// as the docs repos, so this fails the gates when any src/**/*.ts or
// src/**/*.html file carries one of those two code points.
//
// The regex is built from unicode escapes so this file itself stays ASCII
// and never trips its own check.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

const DASH = new RegExp('[\u2013\u2014]', 'g');
const SRC = join(process.cwd(), 'src');

function collectFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...collectFiles(full));
    } else if (entry.name.endsWith('.ts') || entry.name.endsWith('.html')) {
      out.push(full);
    }
  }
  return out;
}

describe('copy guard', () => {
  it('has no em or en dash in src/**/*.ts or src/**/*.html', () => {
    const hits: string[] = [];
    for (const file of collectFiles(SRC)) {
      const lines = readFileSync(file, 'utf8').split('\n');
      lines.forEach((line, i) => {
        DASH.lastIndex = 0;
        if (DASH.test(line)) hits.push(`${file}:${i + 1}`);
      });
    }
    expect(hits).toEqual([]);
  });
});
