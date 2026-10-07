import { BreakpointService } from './breakpoint';

class StubMql {
  private _matches: boolean;
  private readonly listeners: Array<() => void> = [];

  constructor(matches: boolean) {
    this._matches = matches;
  }

  get matches(): boolean {
    return this._matches;
  }

  addEventListener(_type: string, fn: () => void): void {
    this.listeners.push(fn);
  }

  removeEventListener(_type: string, fn: () => void): void {
    const i = this.listeners.indexOf(fn);
    if (i >= 0) this.listeners.splice(i, 1);
  }

  setMatches(v: boolean): void {
    this._matches = v;
    for (const fn of this.listeners) fn();
  }
}

describe('BreakpointService', () => {
  it('defaults to desktop and fine pointers when matchMedia is absent', () => {
    // jsdom ships no window.matchMedia; the constructor must stay quiet.
    const svc = new BreakpointService();
    expect(svc.bp()).toBe('desktop');
    expect(svc.coarse()).toBe(false);
  });

  it('setForTest flips both signals', () => {
    const svc = new BreakpointService();
    svc.setForTest('mobile', true);
    expect(svc.bp()).toBe('mobile');
    expect(svc.coarse()).toBe(true);
    svc.setForTest('tablet', false);
    expect(svc.bp()).toBe('tablet');
    expect(svc.coarse()).toBe(false);
  });

  it('tracks change events from a live matchMedia', () => {
    const stubs = new Map<string, StubMql>();
    const original = window.matchMedia;
    window.matchMedia = (q: string): MediaQueryList => {
      let s = stubs.get(q);
      if (s === undefined) {
        s = new StubMql(false);
        stubs.set(q, s);
      }
      return s as unknown as MediaQueryList;
    };
    try {
      const svc = new BreakpointService();
      // No query matches yet: the tablet region is the remainder.
      expect(svc.bp()).toBe('tablet');
      expect(svc.coarse()).toBe(false);

      stubs.get('(pointer: coarse)')!.setMatches(true);
      expect(svc.coarse()).toBe(true);

      stubs.get('(max-width: 639px)')!.setMatches(true);
      expect(svc.bp()).toBe('mobile');

      stubs.get('(max-width: 639px)')!.setMatches(false);
      stubs.get('(min-width: 1025px)')!.setMatches(true);
      expect(svc.bp()).toBe('desktop');
    } finally {
      if (original === undefined) Reflect.deleteProperty(window, 'matchMedia');
      else window.matchMedia = original;
    }
  });
});
