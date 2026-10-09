import { Model } from './models';
import {
  applyFilters,
  activeFilterCount,
  buildCurl,
  buildRpcCurl,
  CAPABILITY_LABELS,
  clearFilters,
  deriveVendors,
  describeActiveFilters,
  endpointHint,
  modelKind,
  OTHER_VENDOR,
} from './model-filters';

function chat(id: string, name: string, extra: Partial<Model> = {}): Model {
  return {
    id,
    name,
    pricing: { uncachedInput: 10, cachedInput: 1, output: 40 },
    ...extra,
  };
}

describe('deriveVendors', () => {
  it('counts each vendor and groups models without one under "other"', () => {
    const chips = deriveVendors([
      chat('a', 'A', { vendor: 'Meta' }),
      chat('b', 'B', { vendor: 'Meta' }),
      chat('c', 'C', { vendor: 'Jina' }),
      chat('d', 'D'),
      chat('e', 'E'),
    ]);
    expect(chips).toEqual([
      { label: 'Meta', count: 2 },
      { label: 'Jina', count: 1 },
      { label: OTHER_VENDOR, count: 2 },
    ]);
  });

  it('orders count desc then label asc, pinning "other" last even when biggest', () => {
    const chips = deriveVendors([
      chat('a', 'A', { vendor: 'Zeta' }),
      chat('b', 'B', { vendor: 'Alpha' }),
      chat('c', 'C'),
      chat('d', 'D'),
      chat('e', 'E'),
    ]);
    expect(chips.map((c) => c.label)).toEqual(['Alpha', 'Zeta', OTHER_VENDOR]);
  });

  it('derives two chips for two vendors: nothing is hardcoded', () => {
    const chips = deriveVendors([
      chat('a', 'A', { vendor: 'Meta' }),
      chat('b', 'B', { vendor: 'Google' }),
    ]);
    expect(chips).toEqual([
      { label: 'Google', count: 1 },
      { label: 'Meta', count: 1 },
    ]);
  });
});

describe('applyFilters', () => {
  const catalog: Model[] = [
    chat('llama-3.2-3b', 'Llama 3.2 3B', { vendor: 'Meta', contextLength: 32768, servingPath: 'fast', capabilities: ['vision'] }),
    chat('qwen3-8-27b', 'Qwen3 27B', { vendor: 'Alibaba', contextLength: 262144, capabilities: ['function-calling'] }),
    chat('jina-v3', 'Jina v3 Embeddings', { kind: 'embeddings', vendor: 'Jina', contextLength: 524288 }),
    chat('rerank-v1', 'Reranker v1', { kind: 'reranker' }),
  ];

  it('passes everything through with an empty state', () => {
    expect(applyFilters(catalog, clearFilters())).toEqual(catalog);
  });

  it('matches search case-insensitively over name or id', () => {
    expect(applyFilters(catalog, { ...clearFilters(), search: 'LLAMA' }).map((m) => m.id)).toEqual(['llama-3.2-3b']);
    expect(applyFilters(catalog, { ...clearFilters(), search: 'v3' }).map((m) => m.id)).toEqual(['jina-v3']);
  });

  it('reaches no-vendor models only through "other"', () => {
    expect(applyFilters(catalog, { ...clearFilters(), vendors: [OTHER_VENDOR] }).map((m) => m.id)).toEqual(['rerank-v1']);
    expect(applyFilters(catalog, { ...clearFilters(), vendors: ['Meta'] }).map((m) => m.id)).toEqual(['llama-3.2-3b']);
  });

  it('AND-composes two axes: both must match', () => {
    const state = { ...clearFilters(), search: 'qwen', vendors: ['Jina'] };
    expect(applyFilters(catalog, state)).toEqual([]);
    const both = { ...clearFilters(), search: 'qwen', vendors: ['Alibaba'] };
    expect(applyFilters(catalog, both).map((m) => m.id)).toEqual(['qwen3-8-27b']);
  });
});

describe('activeFilterCount / clearFilters / describeActiveFilters', () => {
  it('counts active axes once each', () => {
    expect(activeFilterCount(clearFilters())).toBe(0);
    expect(activeFilterCount({ ...clearFilters(), search: 'x' })).toBe(1);
    expect(activeFilterCount({ ...clearFilters(), search: '   ' })).toBe(0);
    expect(
      activeFilterCount({ ...clearFilters(), search: 'x', vendors: ['Meta', 'Jina'] }),
    ).toBe(2);
  });

  it('clears every axis back to the empty state', () => {
    const cleared = clearFilters();
    expect(cleared).toEqual({
      search: '',
      vendors: [],
    });
  });

  it('names exactly the active axes', () => {
    expect(describeActiveFilters(clearFilters())).toBe('no active filters');
    expect(describeActiveFilters({ ...clearFilters(), search: ' embed ' })).toBe('search "embed"');
    expect(
      describeActiveFilters({
        ...clearFilters(),
        search: 'embed',
        vendors: ['Meta', 'Jina'],
      }),
    ).toBe('search "embed", vendors Meta, Jina');
  });
});

describe('CAPABILITY_LABELS', () => {
  it('labels every wire capability slug', () => {
    expect(CAPABILITY_LABELS).toEqual({
      text: 'Text',
      image: 'Image',
      video: 'Video',
      audio: 'Audio',
      vision: 'Vision',
      'function-calling': 'Function calling',
    });
  });
});

describe('endpointHint', () => {
  it('points each kind at its real endpoint, relative to the base URL', () => {
    expect(endpointHint(chat('a', 'A'))).toBe('/chat/completions');
    expect(endpointHint(chat('e', 'E', { kind: 'embeddings' }))).toBe('/embeddings');
    expect(endpointHint(chat('r', 'R', { kind: 'reranker' }))).toBe('/rerank');
  });
});

describe('buildCurl', () => {
  const base = 'https://wire.example/v1';

  it('builds a chat example with messages and max_tokens', () => {
    const curl = buildCurl(base, chat('llama-3.2-3b', 'Llama'));
    expect(curl).toContain(`curl ${base}/chat/completions`);
    expect(curl).toContain('"model": "llama-3.2-3b"');
    expect(curl).toContain('"messages"');
    expect(curl).toContain('"max_tokens": 1024');
    // The header carries the env var, double-quoted so the shell expands it;
    // no key placeholder survives in the snippet.
    expect(curl).toContain('-H "Authorization: Bearer $SLOTFARE_API_KEY"');
    expect(curl).not.toContain('attk_');
    // The JSON body stays single-quoted so the shell leaves it alone.
    expect(curl).toContain("-d '{");
  });

  it('builds an embeddings example with an input array and no output body', () => {
    const curl = buildCurl(base, chat('jina-v3', 'Jina', { kind: 'embeddings' }));
    expect(curl).toContain(`curl ${base}/embeddings`);
    expect(curl).toContain('"model": "jina-v3"');
    expect(curl).toContain('"input"');
    expect(curl).not.toContain('"messages"');
    expect(curl).not.toContain('"max_tokens"');
    expect(curl).toContain('Bearer $SLOTFARE_API_KEY');
  });

  it('builds a reranker example with query and documents', () => {
    const curl = buildCurl(base, chat('rerank-v1', 'Reranker', { kind: 'reranker' }));
    expect(curl).toContain(`curl ${base}/rerank`);
    expect(curl).toContain('"model": "rerank-v1"');
    expect(curl).toContain('"query"');
    expect(curl).toContain('"documents"');
    expect(curl).not.toContain('"messages"');
  });

  it('keeps the base URL verbatim, including a trailing-slash-free host', () => {
    const curl = buildCurl('http://localhost:8080/v1', chat('a', 'A'));
    expect(curl.startsWith('curl http://localhost:8080/v1/chat/completions')).toBe(true);
  });
});

describe('buildRpcCurl', () => {
  it('builds a metered getHealth call with the same Bearer header', () => {
    const curl = buildRpcCurl('http://localhost:4200/rpc');
    expect(curl).toContain('curl http://localhost:4200/rpc');
    expect(curl).toContain('-H "Authorization: Bearer $SLOTFARE_API_KEY"');
    expect(curl).toContain('"method":"getHealth"');
    expect(curl).toContain("-d '{");
    expect(curl).not.toContain('attk_');
    expect(curl).not.toContain('/v1');
  });
});
