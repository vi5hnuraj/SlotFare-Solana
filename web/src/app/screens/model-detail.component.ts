import { Component, computed, effect, inject } from '@angular/core';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { toSignal } from '@angular/core/rxjs-interop';
import { map } from 'rxjs';
import { ConsoleStore } from '../core/console-store';
import { BreakpointService } from '../core/breakpoint';
import { buildCurl, CAPABILITY_LABELS, KIND_LABELS, modelKind } from '../core/model-filters';
import { ModelCapability } from '../core/models';
import { contextShort, vendorLogo, vendorName } from '../core/model-card';
import { creditText } from '../core/pricing';
import { IconComponent } from '../ui/icon.component';
import { CopyButtonComponent } from '../ui/copy-button.component';
import { SkeletonComponent } from '../ui/skeleton.component';
import { ErrorBlockComponent } from '../ui/error-block.component';
import { EmptyBlockComponent } from '../ui/empty-block.component';

/**
 * The per-model detail view: the full metadata the wire carries, the complete
 * price table in credits per 1M tokens, and a ready-to-copy curl example that
 * reuses the endpoint screen's try-it contract. Absent fields simply do not
 * render; kind and serving path use their documented absent-means defaults so
 * a bare catalog row still reads complete.
 */
@Component({
  selector: 'slf-model-detail',
  imports: [
    RouterLink,
    IconComponent,
    CopyButtonComponent,
    SkeletonComponent,
    ErrorBlockComponent,
    EmptyBlockComponent,
  ],
  templateUrl: './model-detail.component.html',
})
export class ModelDetailComponent {
  protected readonly store = inject(ConsoleStore);
  private readonly route = inject(ActivatedRoute);
  private readonly breakpoints = inject(BreakpointService);
  /** The pinned screen region; the split goes two-column on desktop only. */
  protected readonly bp = this.breakpoints.bp;

  /** The model id named in the URL; the list page links here. */
  protected readonly modelId = toSignal(
    this.route.paramMap.pipe(map((m) => m.get('id'))),
    { initialValue: this.route.snapshot.paramMap.get('id') },
  );

  /** The routed model; null when the catalog does not carry it. */
  protected readonly model = computed(
    () => this.store.models().find((m) => m.id === this.modelId()) ?? null,
  );
  /** True once the catalog has loaded and the URL names no model in it. */
  protected readonly notFound = () =>
    this.modelId() !== null && !this.store.modelsInitial() && !this.store.modelsError() &&
    this.model() === null;

  /** The inference base URL: wire-advertised, or the page origin with /v1. */
  protected readonly baseUrl = computed(() => this.store.endpointBaseUrl());
  /** True when the wire advertises no inference base URL: the catalog is
   *  live but the deployment runs without an inference upstream, so model
   *  calls are disabled and no curl example can work. */
  protected readonly rpcExample = computed(
    () => this.store.advertisedBaseUrl() === '',
  );
  /** The curl example for this model: base URL, real id, kind-fitting body. */
  protected readonly curl = computed(() => {
    const m = this.model();
    return m === null ? '' : buildCurl(this.baseUrl(), m);
  });

  /** The three price rows, rendered from the wire (never hardcoded). */
  protected readonly pricingRows = computed(() => {
    const p = this.model()?.pricing;
    if (p === undefined) return [];
    return [
      { klass: 'Uncached input', credits: creditText(p.uncachedInput) },
      { klass: 'Cached input', credits: creditText(p.cachedInput) },
      { klass: 'Output', credits: creditText(p.output) },
    ];
  });

  protected readonly contextShort = contextShort;
  protected readonly vendorLogo = vendorLogo;
  protected readonly vendorName = vendorName;
  protected readonly creditText = creditText;
  protected readonly capLabel = (c: ModelCapability): string => CAPABILITY_LABELS[c];
  /** Kind label with its documented default: a row without one is chat. */
  protected readonly kindLabel = (): string => {
    const m = this.model();
    return m === null ? '' : KIND_LABELS[modelKind(m)];
  };
  /** Serving path with its documented default; only non-standard is badged. */
  protected readonly servingPathLabel = (): string => {
    const m = this.model();
    if (m === null) return '';
    const p = m.servingPath ?? 'standard';
    return p.charAt(0).toUpperCase() + p.slice(1);
  };
  protected readonly servingPathBadged = (): boolean => {
    const m = this.model();
    return m !== null && m.servingPath !== undefined && m.servingPath !== 'standard';
  };

  constructor() {
    // Keep the keys screen's pick in sync with the routed model.
    effect(() => {
      const m = this.model();
      if (m !== null && this.store.selectedModelId() !== m.id) {
        this.store.setModel(m.id);
      }
    });
  }
}
