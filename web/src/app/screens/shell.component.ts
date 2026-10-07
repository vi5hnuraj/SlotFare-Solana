import { isDirectPurchase } from '../core/trace';
import { Component, DestroyRef, HostListener, computed, effect, inject, signal } from '@angular/core';
import { Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { toSignal } from '@angular/core/rxjs-interop';
import { NavigationEnd, Event } from '@angular/router';
import { filter, map, startWith } from 'rxjs';
import { Subscription } from 'rxjs';
import { ConsoleStore } from '../core/console-store';
import { creditText1, num } from '../core/pricing';
import { shortAddress } from '../core/listing-price';
import { ThemeService } from '../core/theme';
import { BreakpointService } from '../core/breakpoint';
import { IconComponent, IconName } from '../ui/icon.component';
import { ToastComponent } from '../ui/toast.component';

interface NavEntry {
  path: string;
  label: string;
  icon: IconName;
}

const TITLES: Record<string, string> = {
  overview: 'Overview',
  add: 'Add credits',
  funding: 'Transactions',
  sell: 'Sell',
  trade: 'Trade detail',
  market: 'Market',
  keys: 'API keys',
  usage: 'Usage',
  models: 'Models',
};

@Component({
  selector: 'slf-shell',
  imports: [
    RouterOutlet, RouterLink, RouterLinkActive,
    IconComponent, ToastComponent,
  ],
  templateUrl: './shell.component.html',
})
export class ShellComponent {
  private readonly router = inject(Router);
  protected readonly store = inject(ConsoleStore);
  protected readonly theme = inject(ThemeService);
  private readonly breakpoints = inject(BreakpointService);
  protected readonly bp = this.breakpoints.bp;
  /** The navigation drawer is open; only rendered below the desktop region. */
  protected readonly drawerOpen = signal(false);
  /** True once a recovery prompt has shown, so the abandon transition (not a
   *  plain signed-out mount) is what triggers the sign-in navigation. */
  private recoveryWasActive = false;

  /** The chip and subtitle format: the shared short-address helper, with the
   *  raw value when the helper cannot shorten it. */
  protected readonly chipAddress = (a: string): string =>
    shortAddress(a) !== '' ? shortAddress(a) : a;

  protected readonly primaryNav: NavEntry[] = [
    { path: '/overview', label: 'Overview', icon: 'grid' },
    { path: '/add', label: 'Add credits', icon: 'plus-circle' },
    { path: '/sell', label: 'Sell', icon: 'shield' },
    { path: '/market', label: 'Market', icon: 'grid' },
    { path: '/funding', label: 'Transactions', icon: 'route' },
  ];
  protected readonly buildNav: NavEntry[] = [
    { path: '/models', label: 'Models', icon: 'cube' },
    { path: '/keys', label: 'API keys', icon: 'key' },
    { path: '/usage', label: 'Usage', icon: 'bars' },
  ];

  protected disconnect(): void {
    this.store.disconnect();
    void this.router.navigate(['/connect']);
  }

  /** Ask for a fresh wallet signature in place (the recovery prompt's primary action). */
  protected reauth(): void {
    this.store.reauth();
  }

  /** Leave the recovery prompt for the connect screen to pick a different wallet. */
  protected chooseWallet(): void {
    void this.router.navigate(['/connect']);
  }

  constructor() {
    // A fresh page is a closed drawer: navigating away never leaves it up.
    inject(DestroyRef).onDestroy(() => this.navCloseSub.unsubscribe());

    // When recovery is abandoned (declined or no wallet) - the prompt drops
    // while still signed out - land on the sign-in screen cleanly. This fires
    // only on the true->false transition of `sessionExpired`, so a plain
    // signed-out mount never triggers it. While `sessionExpired` is true the
    // shell stays mounted on the current route, so the prompt shows in place.
    effect(() => {
      const expired = this.store.sessionExpired();
      if (expired) {
        this.recoveryWasActive = true;
        return;
      }
      if (this.recoveryWasActive && !this.store.signedIn()) {
        this.recoveryWasActive = false;
        void this.router.navigate(['/connect']);
      }
    });
  }
  private readonly url = toSignal(
    this.router.events.pipe(
      filter((e: Event): e is NavigationEnd => e instanceof NavigationEnd),
      map((e) => e.urlAfterRedirects),
      startWith(this.router.url),
    ),
    { initialValue: this.router.url },
  );

  /**
   * Subscribes to the NavigationEnd stream directly rather than through an
   * effect: an effect body that read drawerOpen() would track it as a
   * dependency and re-close the drawer the instant it opened after the
   * first navigation.
   */
  private readonly navCloseSub = this.router.events
    .pipe(filter((e: Event) => e instanceof NavigationEnd))
    .subscribe(() => this.closeDrawer());

  protected toggleDrawer(): void {
    this.drawerOpen.update((o) => !o);
    this.syncBodyLock();
  }

  protected closeDrawer(): void {
    if (this.drawerOpen()) {
      this.drawerOpen.set(false);
      this.syncBodyLock();
    }
  }

  /** While the drawer covers the page, the page behind must not scroll. */
  private syncBodyLock(): void {
    document.body.style.overflow = this.drawerOpen() ? 'hidden' : '';
  }

  @HostListener('keydown.escape')
  closeDrawerOnEscape(): void {
    this.closeDrawer();
  }

  private readonly segment = computed(() => this.url().split('/')[1] || 'overview');
  /** The purchase id when the URL is /funding/:id, else null. */
  private readonly routedPurchase = computed(() => {
    const parts = this.url().split('?')[0].split('/');
    return parts[1] === 'funding' && parts[2] ? parts[2] : null;
  });
  /** The trade id when the URL is /trade/:id, else null. */
  private readonly routedTrade = computed(() => {
    const parts = this.url().split('?')[0].split('/');
    return parts[1] === 'trade' && parts[2] ? parts[2] : null;
  });
  /** The model id when the URL is /models/:id, else null. */
  private readonly routedModel = computed(() => {
    const parts = this.url().split('?')[0].split('/');
    return parts[1] === 'models' && parts[2] ? parts[2] : null;
  });
  protected readonly title = computed(() => {
    if (this.routedPurchase() !== null) return 'Funding trace';
    if (this.routedTrade() !== null) return 'Trade detail';
    return TITLES[this.segment()] ?? TITLES['overview'];
  });
  /** The topbar subtitle is derived from the store, never hardcoded. */
  protected readonly subtitle = computed(() => {
    switch (this.segment()) {
      case 'add':
        return 'Deposit USDC on Solana';
      case 'funding': {
        const id = this.routedPurchase();
        if (id !== null) {
          const st = this.store.fundingStatus();
          if (st !== null && st.purchaseId === id) {
            if (isDirectPurchase(st)) return `Purchase ${id} · ${st.chainName || 'Solana'}`;
            return `Purchase ${id} · ${st.chainName || 'source chain'}`;
          }
          return `Purchase ${id}`;
        }
        const n = this.store.transactionCount();
        return `${n} transaction${n === 1 ? '' : 's'}`;
      }
      case 'trade': {
        const id = this.routedTrade();
        if (id === null) return '';
        const l = this.store.listings().find((x) => x.id === id);
        if (l === undefined) return `Listing ${id}`;
        return `Listing ${id} · ${l.state === 'sold' ? 'Sold' : 'Cancelled'}`;
      }
      case 'keys': {
        const active = this.store.activeKeys().length;
        const revoked = this.store.keys().length - active;
        return `${active} active · ${revoked} revoked`;
      }
      case 'usage':
        return `Last ${this.store.usageDays()} days`;
      case 'sell': {
        const n = this.store.openListingsCount();
        return `${n} open · ${creditText1(this.store.locked())} credits locked`;
      }
      case 'market': {
        const forSale = this.store.forSaleCount();
        const buyable = this.store.buyableCount();
        if (forSale === buyable) return `${num(forSale)} for sale`;
        return `${num(forSale)} for sale, ${num(buyable)} buyable for you`;
      }
      case 'models': {
        const id = this.routedModel();
        if (id !== null) {
          const m = this.store.models().find((x) => x.id === id);
          return m !== undefined ? m.name : id;
        }
        const n = this.store.models().length;
        return `${n} model${n === 1 ? '' : 's'}`;
      }
      default:
        return `Credit account ${this.chipAddress(this.store.address())}`;
    }
  });
}
