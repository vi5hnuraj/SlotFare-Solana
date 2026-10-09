import { existsSync } from 'node:fs';
import { Model } from './models';
import { contextShort, pricingLines, vendorLogo } from './model-card';

function model(partial: Partial<Model> = {}): Model {
  return {
    id: 'm-1',
    name: 'Model One',
    pricing: { uncachedInput: 10, cachedInput: 1, output: 40 },
    ...partial,
  };
}

describe('contextShort', () => {
  it('humanizes below a million to rounded k', () => {
    expect(contextShort(32768)).toBe('33k');
    expect(contextShort(131072)).toBe('131k');
    expect(contextShort(262144)).toBe('262k');
    expect(contextShort(524288)).toBe('524k');
  });

  it('humanizes at and above a million to decimal-based M', () => {
    expect(contextShort(1_000_000)).toBe('1M');
    expect(contextShort(1_300_000)).toBe('1.3M');
    expect(contextShort(2_048_000)).toBe('2M');
  });
});

describe('pricingLines', () => {
  it('gives chat models three lines in wire order, value then label', () => {
    expect(
      pricingLines(model({ pricing: { uncachedInput: 1320, cachedInput: 44, output: 3960 } })),
    ).toEqual([
      { value: '1320 credits / M', label: 'uncached' },
      { value: '44 credits / M', label: 'cached' },
      { value: '3960 credits / M', label: 'output' },
    ]);
  });

  it('treats an absent kind as chat', () => {
    const lines = pricingLines(model({}));
    expect(lines.map((l) => l.label)).toEqual(['uncached', 'cached', 'output']);
  });

  it('gives embeddings and rerankers their single input line only', () => {
    expect(pricingLines(model({ kind: 'embeddings' }))).toEqual([
      { value: '10 credits / M', label: 'uncached' },
    ]);
    expect(pricingLines(model({ kind: 'reranker' }))).toEqual([
      { value: '10 credits / M', label: 'uncached' },
    ]);
  });

  it('keeps fractional credit values visible', () => {
    expect(pricingLines(model({ pricing: { uncachedInput: 0.5, cachedInput: 0, output: 0 } }))[0]).toEqual(
      { value: '0.5 credits / M', label: 'uncached' },
    );
  });
});

describe('vendorLogo', () => {
  const slugs = [
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
  ];

  it('resolves every bundled slug, case-insensitively', () => {
    for (const s of slugs) {
      expect(vendorLogo(s)).toBe(`vendor-logos/${s}.svg`);
    }
    expect(vendorLogo('Moonshot')).toBe('vendor-logos/moonshot.svg');
  });

  it('maps the testnet catalog display names onto their slugs', () => {
    expect(vendorLogo('Mistral')).toBe('vendor-logos/mistralai.svg');
    expect(vendorLogo('Google')).toBe('vendor-logos/google.svg');
    expect(vendorLogo('Meta')).toBe('vendor-logos/meta.svg');
    expect(vendorLogo('Alibaba')).toBe('vendor-logos/qwen.svg');
  });

  it('resolves a slug or alias buried in whitespace or a multi-word name', () => {
    expect(vendorLogo(' google ')).toBe('vendor-logos/google.svg');
    expect(vendorLogo('Google DeepMind')).toBe('vendor-logos/google.svg');
    expect(vendorLogo('Mistral AI')).toBe('vendor-logos/mistralai.svg');
    // Token matches are exact: a name that merely starts with a slug stays unknown.
    expect(vendorLogo('Jina')).toBeUndefined();
  });

  it('returns undefined for unknown or absent vendors', () => {
    expect(vendorLogo('')).toBeUndefined();
    expect(vendorLogo('   ')).toBeUndefined();
    expect(vendorLogo(undefined)).toBeUndefined();
  });

  it('bundles one local SVG per slug', () => {
    for (const s of slugs) {
      expect(existsSync(`public/vendor-logos/${s}.svg`)).toBe(true);
    }
  });
});
