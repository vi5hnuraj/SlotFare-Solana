import { AfterViewInit, Component, DestroyRef, ElementRef, computed, input, signal } from '@angular/core';
import { creditText, usd } from '../core/pricing';

interface Bar {
  index: number;
  x: number;
  y: number;
  width: number;
  height: number;
  weekend: boolean;
  label: string;
  title: string;
}

const W = 900;
const H = 190;
const PAD_L = 46;
const PAD_B = 26;
const PAD_T = 10;
/** Axis floor when there is no spend yet: 1k reads as a scale, 40k read as a lie. */
const MIN_CEILING = 1_000;

/** The smallest 1-2-5 x 10^n ceiling at or above v, so the top grid line is a round number. */
export function niceCeiling(v: number): number {
  if (!(v > 0)) return MIN_CEILING;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 5, 10]) {
    if (m * exp >= v) return Math.max(MIN_CEILING, m * exp);
  }
  return Math.max(MIN_CEILING, 10 * exp);
}

/** Daily credit spend. Plain SVG rather than a charting dependency: it is
 *  thirty bars and a mean line, and the CSS already themes it. */
@Component({
  selector: 'slf-spend-chart',
  templateUrl: './spend-chart.component.html',
})
export class SpendChartComponent implements AfterViewInit {
  readonly values = input.required<number[]>();
  readonly dayZero = input.required<number>();

  /** Measured width of the card in px. Public on purpose: it is the test
   *  seam for the sizing hooks, and jsdom has no ResizeObserver. */
  readonly containerWidth = signal<number>(W);

  private ro: ResizeObserver | null = null;

  constructor(
    private readonly el: ElementRef<HTMLElement>,
    private readonly destroyRef: DestroyRef,
  ) {}

  ngAfterViewInit(): void {
    // Guarded like BreakpointService's matchMedia: jsdom and SSR stay quiet.
    if (typeof ResizeObserver === 'undefined') return;
    // The host element is display:inline and measures 0px wide in a real
    // browser, so observe the block .chart div inside the view instead.
    const node = this.el.nativeElement.querySelector('.chart');
    if (!(node instanceof Element)) return;
    this.ro = new ResizeObserver((entries) => {
      for (const entry of entries) {
        const w = entry.contentRect.width;
        if (w > 0) this.containerWidth.set(Math.round(w));
      }
    });
    this.ro.observe(node);
    this.destroyRef.onDestroy(() => this.ro?.disconnect());
  }

  protected readonly viewBox = `0 0 ${W} ${H}`;
  protected readonly width = W;
  protected readonly hovered = signal<Bar | null>(null);

  /** Rendered scale of the viewBox: 1 at the native 900px, below on narrow cards. */
  protected readonly scale = computed(() => this.containerWidth() / W);

  /** Axis font in viewBox units, counter-scaled so the rendered size stays
   *  ~11.5px whenever the container is narrower than the viewBox; wide
   *  screens keep the CSS default and never grow the font. */
  protected readonly axisFont = computed(() =>
    Math.round((Math.max(11.5, 11.5 / Math.min(this.scale(), 1)) as number) * 10) / 10,
  );

  /** Every Nth bar carries an x-label. 56px is the minimum rendered gap
   *  between "MM-DD" labels; the floor of 6 keeps today's desktop density.
   *  Labels thin out only when the container gets narrow. */
  protected readonly axisStep = computed(() => {
    const n = this.values().length;
    if (n === 0) return 1;
    const innerPx = Math.max(this.containerWidth() - (PAD_L + 8) * this.scale(), 1);
    const perBar = innerPx / n;
    return Math.max(6, Math.ceil(56 / perBar));
  });

  /** Top of the axis: a round ceiling just above the busiest day. */
  protected readonly ceiling = computed(() => niceCeiling(Math.max(0, ...this.values())));

  /** True when the window holds no spend at all; the chart then shows an empty state. */
  protected readonly empty = computed(() => this.values().every((v) => v === 0));

  protected readonly gridLines = computed(() => {
    const top = this.ceiling();
    return [0, 0.25, 0.5, 0.75, 1].map((f) => {
      const v = top * f;
      return {
        value: v,
        y: this.y(v),
        label: v >= 1000 ? `${Math.round((v / 1000) * 10) / 10}k` : `${Math.round(v)}`,
      };
    });
  });

  protected readonly bars = computed<Bar[]>(() => {
    const vals = this.values();
    const inner = W - PAD_L - 8;
    const bw = inner / vals.length;
    return vals.map((v, i) => {
      const date = new Date(this.dayZero() + i * 86_400_000);
      const day = date.getUTCDay();
      const iso = date.toISOString().slice(0, 10);
      return {
        index: i,
        x: PAD_L + i * bw + 1.5,
        y: this.y(v),
        width: bw - 3,
        height: H - PAD_B - this.y(v),
        weekend: day === 0 || day === 6,
        label: iso.slice(5),
        title: `${iso} · ${creditText(v)} credits · ${usd(v)}`,
      };
    });
  });

  protected readonly axisLabels = computed(() =>
    this.bars()
      .filter((b, i) => i % this.axisStep() === 0 || i === this.bars().length - 1)
      .map((b) => ({ x: b.x + b.width / 2, y: H - PAD_B + 15, text: b.label })),
  );

  protected readonly mean = computed(() => {
    const vals = this.values();
    return vals.reduce((a, b) => a + b, 0) / vals.length;
  });

  protected readonly meanY = computed(() => this.y(this.mean()));
  protected readonly meanLabel = computed(() => `mean ${creditText(this.mean())}`);

  protected readonly description = computed(() => {
    const total = this.values().reduce((a, b) => a + b, 0);
    return `Daily credit spend for the last ${this.values().length} days. Total ${creditText(total)} credits, ${usd(total)}. Daily mean ${creditText(this.mean())} credits. Weekend days are consistently lower.`;
  });

  protected readonly tipStyle = computed(() => {
    const b = this.hovered();
    if (!b) return null;
    return {
      left: `${((b.x + b.width / 2) / W) * 100}%`,
      top: `${(b.y / H) * 100}%`,
    };
  });

  protected readonly tipText = computed(() => this.hovered()?.title ?? '');

  private y(v: number): number {
    return PAD_T + (H - PAD_T - PAD_B) * (1 - v / this.ceiling());
  }

  protected readonly padLeft = PAD_L;
  protected readonly axisTextX = PAD_L - 8;
}
