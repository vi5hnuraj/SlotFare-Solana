/**
 * The model list's surviving filter and derivation logic, pure and store-free:
 * the two browse axes the dedicated Models page exposes (search and vendor
 * chips) plus the per-model helpers the detail screen prices against. Nothing
 * here hardcodes a vendor list or a catalog; two models yield two chips today,
 * a mainnet catalog yields its own.
 */
import { Model, ModelCapability, ModelKind } from './models';

/** One axis of the composed filter; an empty array means "no restriction". */
export interface ModelFilterState {
  search: string;
  /** Vendor labels as derived by `deriveVendors`, including `other`. */
  vendors: string[];
}

/** A vendor chip: the label and how many of the served models carry it. */
export interface VendorChip {
  label: string;
  count: number;
}

/** The label models without a wire vendor group under. */
export const OTHER_VENDOR = 'other';

/** Display names for the type facet and the empty-state wording. */
export const KIND_LABELS: Record<ModelKind, string> = {
  chat: 'Chat',
  embeddings: 'Embeddings',
  reranker: 'Reranker',
};

/** Display names for capability badges. */
export const CAPABILITY_LABELS: Record<ModelCapability, string> = {
  text: 'Text',
  image: 'Image',
  video: 'Video',
  audio: 'Audio',
  vision: 'Vision',
  'function-calling': 'Function calling',
};

/** The kind a model serves; a row without one is chat-shaped copy. */
export function modelKind(m: Model): ModelKind {
  return m.kind ?? 'chat';
}

/** The distinct vendors present, each with its model count; models without
 *  a vendor group under `other`. Ordered count desc, then label asc, with
 *  `other` pinned last even when it has the biggest count. */
export function deriveVendors(models: Model[]): VendorChip[] {
  const counts = new Map<string, number>();
  for (const m of models) {
    const label = m.vendor ?? OTHER_VENDOR;
    counts.set(label, (counts.get(label) ?? 0) + 1);
  }
  const rows = [...counts.entries()].map(([label, count]) => ({ label, count }));
  rows.sort((a, b) => b.count - a.count || a.label.localeCompare(b.label));
  return [
    ...rows.filter((r) => r.label !== OTHER_VENDOR),
    ...rows.filter((r) => r.label === OTHER_VENDOR),
  ];
}

/** The single-pass AND-composition of every active axis. A model without a
 *  value on an active axis (no vendor) is excluded by that axis. */
export function applyFilters(models: Model[], state: ModelFilterState): Model[] {
  const q = state.search.trim().toLowerCase();
  return models.filter((m) => {
    if (
      q !== '' &&
      !(m.name.toLowerCase().includes(q) || m.id.toLowerCase().includes(q))
    ) {
      return false;
    }
    if (state.vendors.length > 0 && !state.vendors.includes(m.vendor ?? OTHER_VENDOR)) {
      return false;
    }
    return true;
  });
}

/** How many axes are active; search counts once, vendor counts once when its
 *  array is non-empty. Drives the "N filters active" bar. */
export function activeFilterCount(state: ModelFilterState): number {
  let n = 0;
  if (state.search.trim() !== '') n += 1;
  if (state.vendors.length > 0) n += 1;
  return n;
}

/** A fresh empty state; the one-action clear. */
export function clearFilters(): ModelFilterState {
  return {
    search: '',
    vendors: [],
  };
}

/** Names exactly the axes that are active, for the empty-result line:
 *  e.g. `search "embed", vendors Meta, Jina`. */
export function describeActiveFilters(state: ModelFilterState): string {
  const parts: string[] = [];
  const q = state.search.trim();
  if (q !== '') parts.push(`search "${q}"`);
  if (state.vendors.length > 0) {
    parts.push(`${state.vendors.length === 1 ? 'vendor' : 'vendors'} ${state.vendors.join(', ')}`);
  }
  return parts.length > 0 ? parts.join(', ') : 'no active filters';
}

/** The endpoint path a kind is called on, relative to the advertised base
 *  URL (which already carries the /v1 prefix): chat keeps the OpenAI-standard
 *  shape, embeddings and rerankers get their real paths. */
export function endpointHint(m: Model): string {
  switch (modelKind(m)) {
    case 'embeddings':
      return '/embeddings';
    case 'reranker':
      return '/rerank';
    default:
      return '/chat/completions';
  }
}

/**
 * A ready-to-copy curl example for a model: the wire-advertised base URL,
 * the model's real id, and a body that fits its kind. The header references
 * `$SLOTFARE_API_KEY`, which the user exports once from one of their keys;
 * double quotes keep the shell expanding it while the JSON body stays
 * single-quoted.
 */
export function buildCurl(baseUrl: string, m: Model): string {
  const url = `${baseUrl}${endpointHint(m)}`;
  const head = `curl ${url} \\\n  -H "Authorization: Bearer $SLOTFARE_API_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '{`;
  switch (modelKind(m)) {
    case 'embeddings':
      return `${head}\n    "model": "${m.id}",\n    "input": [\n      "What is the capital of Denmark?",\n      "Slotfare meters inference spend in credits"\n    ]\n  }'`;
    case 'reranker':
      return `${head}\n    "model": "${m.id}",\n    "query": "credit metering gateway",\n    "documents": [\n      "Slotfare meters inference spend in credits",\n      "A USDC deposit on Solana mints credits",\n      "Settlement commits a usage root to Solana"\n    ]\n  }'`;
    default:
      return `${head}\n    "model": "${m.id}",\n    "messages": [\n      { "role": "user", "content": "Summarise this changelog in five bullets." }\n    ],\n    "max_tokens": 1024\n  }'`;
  }
}

/**
 * A ready-to-copy curl for deployments without an inference upstream: a
 * metered Devnet JSON-RPC call through the /rpc proxy, same Bearer key and
 * same charge headers as inference. The body is a fixed getHealth so the
 * example runs anywhere without edits.
 */
export function buildRpcCurl(baseUrl: string): string {
  return (
    `curl ${baseUrl} \\\n` +
    '  -H "Authorization: Bearer $SLOTFARE_API_KEY" \\\n' +
    '  -H "Content-Type: application/json" \\\n' +
    `  -d '{"jsonrpc":"2.0","id":1,"method":"getHealth"}'`
  );
}
