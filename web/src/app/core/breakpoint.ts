import { Injectable, signal } from '@angular/core';

export type Breakpoint = 'mobile' | 'tablet' | 'desktop';

/**
 * The three pinned screen regions of the console, shared with the SCSS
 * tokens $fb-mobile / $fb-tablet in styles/slotfare.scss:
 * mobile under 640, tablet 640 to 1024, desktop above.
 */
@Injectable({ providedIn: 'root' })
export class BreakpointService {
  /** 'desktop' without a live media query: the full sidebar, never a drawer. */
  readonly bp = signal<Breakpoint>('desktop');
  readonly coarse = signal(false);

  private mobileQ: MediaQueryList | null = null;
  private desktopQ: MediaQueryList | null = null;
  private coarseQ: MediaQueryList | null = null;

  constructor() {
    const win = typeof window !== 'undefined' ? window : undefined;
    if (win === undefined || win.matchMedia === undefined) return;
    this.mobileQ = win.matchMedia('(max-width: 639px)');
    this.desktopQ = win.matchMedia('(min-width: 1025px)');
    this.coarseQ = win.matchMedia('(pointer: coarse)');
    this.sync();
    this.mobileQ.addEventListener('change', () => this.sync());
    this.desktopQ.addEventListener('change', () => this.sync());
    this.coarseQ.addEventListener('change', () => this.sync());
  }

  /** Test seam: specs set the regions directly and never need matchMedia. */
  setForTest(bp: Breakpoint, coarse: boolean): void {
    this.bp.set(bp);
    this.coarse.set(coarse);
  }

  private sync(): void {
    const m = this.mobileQ;
    const d = this.desktopQ;
    const c = this.coarseQ;
    if (m === null || d === null || c === null) return;
    this.bp.set(m.matches ? 'mobile' : d.matches ? 'desktop' : 'tablet');
    this.coarse.set(c.matches);
  }
}
