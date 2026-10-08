import { Injectable, inject } from '@angular/core';
import { DestroyRef } from '@angular/core';
import { Subject } from 'rxjs';

/**
 * The one cadence every live figure refreshes on while the user is signed in
 * and the tab is visible: balances, overview cards, the request log,
 * transactions, market listings and funding traces.
 */
export const LIVE_POLL_MS = 15_000;

/**
 * The console-wide polling loop. It owns the single interval and the
 * document-visibility gating; it knows nothing about sessions or reads.
 *
 * The tick is a bare subject: the interval callback only emits it, so it can
 * never throw and can never be stopped by a subscriber's failure. That is what
 * makes the loop immortal - no error, however repeated, can halt it. Screens
 * subscribe through the store; they never own their own timers.
 */
@Injectable({ providedIn: 'root' })
export class PollingService {
  /** Emitted on every tick. Subscribers drive their reads from this. */
  readonly tick = new Subject<void>();

  private readonly destroyRef = inject(DestroyRef);
  private timer: ReturnType<typeof setInterval> | undefined;
  private enabled = false;
  private readonly onVisibilityChange = () => this.handleVisibility();

  constructor() {
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', this.onVisibilityChange);
      this.destroyRef.onDestroy(() => {
        document.removeEventListener('visibilitychange', this.onVisibilityChange);
        this.clearTimer();
      });
    }
  }

  /** The document's visibility, true in non-DOM contexts. */
  private visible(): boolean {
    return typeof document === 'undefined' || !document.hidden;
  }

  private clearTimer(): void {
    if (this.timer !== undefined) {
      clearInterval(this.timer);
      this.timer = undefined;
    }
  }

  /** Arm the interval from now, replacing any existing one. */
  private runTimer(): void {
    this.clearTimer();
    this.timer = setInterval(() => this.tick.next(), LIVE_POLL_MS);
  }

  /**
   * Arm the loop. Idempotent: a repeat restarts cleanly. While enabled, a
   * hidden tab holds no interval; a visible one resumes it.
   */
  start(): void {
    this.enabled = true;
    if (this.visible()) this.runTimer();
  }

  /** Disarm the loop and drop any interval. Safe when idle. */
  stop(): void {
    this.enabled = false;
    this.clearTimer();
  }

  private handleVisibility(): void {
    if (!this.enabled) return;
    if (this.visible()) {
      // Catch up immediately, then resume the ordinary interval.
      this.tick.next();
      this.runTimer();
    } else {
      this.clearTimer();
    }
  }
}
