import { Model } from './models';
import { modelKind } from './model-filters';

/**
 * The Models list card's presentation layer: the humanized context line,
 * the stacked pricing column and the bundled vendor logo. Pure functions
 * over wire data, so the card's markup stays a thin template.
 */

/**
 * The wire's credit figure, plain: micro-credit resolution kept, no
 * thousands grouping, so 1320 reads "1320 credits / M", not "1,320 credits / M".
 */
const nfFig = new Intl.NumberFormat('en-US', {
  useGrouping: false,
  maximumFractionDigits: 4,
});

/**
 * A context window in tokens, humanized for the card's one-line read:
 * decimal-based k/M, so 262144 reads "262k" and 1300000 reads "1.3M".
 */
export function contextShort(n: number): string {
  if (n >= 1_000_000) {
    const m = n / 1e6;
    return (Math.round(m * 10) / 10).toString().replace(/\.0$/, '') + 'M';
  }
  return Math.round(n / 1e3) + 'k';
}

export interface PricingLine {
  value: string;
  label: string;
}

/**
 * The card's stacked right-hand pricing column: value then muted label,
 * one line per component. Chat models carry all three lines in order;
 * embeddings and rerankers show their single input line only.
 */
export function pricingLines(m: Model): PricingLine[] {
  const p = m.pricing;
  if (modelKind(m) !== 'chat') {
    return [
      { value: nfFig.format(p.uncachedInput) + ' credits / M', label: 'uncached' },
    ];
  }
  return [
    { value: nfFig.format(p.uncachedInput) + ' credits / M', label: 'uncached' },
    { value: nfFig.format(p.cachedInput) + ' credits / M', label: 'cached' },
    { value: nfFig.format(p.output) + ' credits / M', label: 'output' },
  ];
}

/** Every slug with a bundled logo under public/vendor-logos/. */
const LOGO_SLUGS = [
  'mistralai',
  'google',
  'moonshot',
  'zai',
  'minimax',
  'deepseek',
  'nvidia',
  'qwen',
  'openai',
  'meta',
  'thinking-machines',
] as const;

type LogoSlug = (typeof LOGO_SLUGS)[number];

/** Display names the testnet catalog carries that map onto a slug. */
const VENDOR_ALIASES: Record<string, LogoSlug> = {
  mistral: 'mistralai',
  google: 'google',
  meta: 'meta',
  alibaba: 'qwen',
};

const SLUG_SET = new Set<string>(LOGO_SLUGS);

/** How each vendor writes its own name; falls back to the wire value. */
const VENDOR_NAMES: Record<string, string> = {
  openai: 'OpenAI',
  nvidia: 'NVIDIA',
  deepseek: 'DeepSeek',
  moonshot: 'Moonshot AI',
  qwen: 'Qwen',
  google: 'Google',
  meta: 'Meta',
  mistralai: 'Mistral AI',
  minimax: 'MiniMax',
  zai: 'Z.ai',
  'thinking-machines': 'Thinking Machines',
  other: 'Other',
};

/**
 * The vendor's display name for chips, cards and the detail head. Resolves
 * through the same slug and alias table as the logo, so "alibaba" reads
 * "Qwen"; anything unknown is shown as the wire value untouched.
 */
export function vendorName(vendor?: string): string {
  if (vendor === undefined || vendor.trim() === '') return '';
  const v = vendor.trim().toLowerCase();
  const slug = SLUG_SET.has(v) ? v : VENDOR_ALIASES[v];
  return VENDOR_NAMES[slug ?? v] ?? vendor;
}

/**
 * The local logo path for a wire vendor slug or display name, or undefined
 * when no logo is bundled and the card falls back to its letter avatar.
 * Assets live under public/vendor-logos/ and are never fetched from the
 * network. Resolution order: exact slug, then alias, then any single token
 * of a multi-word name that is itself a known slug or alias (so wire values
 * like "Google DeepMind" still hit the bundled google mark). Multi-word
 * names only ever resolve through a token-exact match; nothing else falls
 * through to the letter avatar.
 */
export function vendorLogo(vendor?: string): string | undefined {
  if (vendor === undefined || vendor === '') return undefined;
  const v = vendor.trim().toLowerCase();
  if (v === '') return undefined;
  const resolve = (s: string): LogoSlug | undefined =>
    SLUG_SET.has(s) ? (s as LogoSlug) : VENDOR_ALIASES[s];
  const whole = resolve(v);
  if (whole !== undefined) return 'vendor-logos/' + whole + '.svg';
  const tokens = v.split(/[\s\-_.]+/).filter((t) => t !== '');
  for (const t of tokens) {
    const hit = resolve(t);
    if (hit !== undefined) return 'vendor-logos/' + hit + '.svg';
  }
  return undefined;
}
