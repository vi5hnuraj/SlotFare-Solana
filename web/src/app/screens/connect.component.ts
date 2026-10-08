import { Component, computed, effect, inject, signal } from '@angular/core';
import { Router } from '@angular/router';
import { ConsoleStore } from '../core/console-store';
import { ThemeService } from '../core/theme';
import { WalletService } from '../core/wallet';
import { IconComponent } from '../ui/icon.component';
import { ErrorBlockComponent } from '../ui/error-block.component';
import { FireworksComponent } from '../ui/fireworks.component';

@Component({
  selector: 'slf-connect',
  imports: [IconComponent, ErrorBlockComponent, FireworksComponent],
  templateUrl: './connect.component.html',
})
export class ConnectComponent {
  private readonly store = inject(ConsoleStore);
  private readonly router = inject(Router);
  protected readonly theme = inject(ThemeService);
  protected readonly walletSvc = inject(WalletService);

  /** The rdns of the wallet the user clicked; null until they do. */
  private readonly selectedRdns = signal<string | null>(null);
  /** The last rdns attempted, so Retry can repeat it after a failure. */
  private readonly lastRdns = signal<string | null>(null);

  /** The address the handshake has already been started for, so a re-run of
   *  the effect below never starts a second one (or retries a failed one). */
  private announcedFor: string | null = null;

  /** The address is known and the sign-in message is out for signature. */
  protected readonly signing = computed(
    () => this.walletSvc.address() !== null && this.walletSvc.session() === null && this.walletSvc.error() === null,
  );

  /** Icons that failed to load, so their letter avatar stands in instead.
   *  A provider may announce a broken or empty icon; the picker must not
   *  render a dead image for it. */
  protected readonly iconFailed = signal<Set<string>>(new Set());

  /** Record a broken wallet icon so the template falls back to the letter. */
  protected markIconFailed(icon: string): void {
    this.iconFailed.update((set) => {
      const next = new Set(set);
      next.add(icon);
      return next;
    });
  }

  constructor() {
    // Address in hand: start the sign-in. Nothing navigates yet; an address
    // alone is not an identity.
    effect(() => {
      const addr = this.walletSvc.address();
      const rdns = this.selectedRdns();
      if (addr === null || rdns === null || addr === this.announcedFor) return;
      this.announcedFor = addr;
      const p = this.walletSvc.providers().find((w) => w.rdns === rdns);
      this.store.connect(
        p ?? { name: 'Wallet', rdns, initial: '?' },
      );
    });
    // Session verified: the console can be served.
    effect(() => {
      if (this.store.signedIn()) void this.router.navigate(['/overview']);
    });
  }

  protected connect(rdns: string): void {
    this.announcedFor = null;
    this.selectedRdns.set(rdns);
    this.lastRdns.set(rdns);
    this.walletSvc.connect(rdns);
  }

  /** Retry from the error block: the signature when the address is already
   *  known, the whole connection otherwise. */
  protected retryConnect(): void {
    if (this.walletSvc.address() !== null) {
      this.walletSvc.startSession();
      return;
    }
    const r = this.lastRdns();
    if (r !== null) this.connect(r);
  }
}
