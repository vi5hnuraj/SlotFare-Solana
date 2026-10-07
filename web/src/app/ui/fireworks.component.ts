import { Component, DestroyRef, ElementRef, afterNextRender, inject, viewChild } from '@angular/core';

/** One particle of a burst. Positions in CSS pixels, velocities per frame. */
interface Particle {
  x: number;
  y: number;
  /** Previous position, so each frame draws a short streak, not a dot. */
  px: number;
  py: number;
  vx: number;
  vy: number;
  life: number;
  ttl: number;
  color: string;
  size: number;
}

/** The brand palette, in the order bursts cycle through it. */
const COLORS = ['#f45b35', '#e9ff70', '#a7f3bd', '#f45b35', '#e9ff70'];

/**
 * A temporary celebration layer for the connect screen: a few seconds of
 * fireworks in the brand palette on a fixed canvas behind the card, then
 * nothing. It renders nothing at all under prefers-reduced-motion and
 * removes itself when the show ends, so the page is unchanged afterwards.
 *
 * Temporary by design. Delete this file, its selector in
 * connect.component.html, and the `.fireworks` rule in slotfare.scss to
 * remove it.
 */
@Component({
  selector: 'slf-fireworks',
  template: '<canvas #c class="fireworks" aria-hidden="true"></canvas>',
})
export class FireworksComponent {
  private readonly canvas = viewChild.required<ElementRef<HTMLCanvasElement>>('c');
  private readonly destroyRef = inject(DestroyRef);

  /** How long bursts keep launching, in milliseconds. */
  private static readonly SHOW_MS = 7000;
  /** Gap between launches, in milliseconds. */
  private static readonly LAUNCH_EVERY_MS = 550;

  private raf = 0;
  private particles: Particle[] = [];
  private startedAt = 0;
  private lastLaunch = 0;
  private burstIndex = 0;

  constructor() {
    afterNextRender(() => this.start());
    this.destroyRef.onDestroy(() => this.stop());
  }

  private start(): void {
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      this.remove();
      return;
    }
    const el = this.canvas().nativeElement;
    const ctx = el.getContext('2d');
    if (ctx === null) {
      this.remove();
      return;
    }
    ctx.lineCap = 'round';
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const fit = (): void => {
      el.width = Math.floor(window.innerWidth * dpr);
      el.height = Math.floor(window.innerHeight * dpr);
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    };
    fit();
    window.addEventListener('resize', fit);
    this.destroyRef.onDestroy(() => window.removeEventListener('resize', fit));

    this.startedAt = performance.now();
    this.lastLaunch = this.startedAt - FireworksComponent.LAUNCH_EVERY_MS;
    const frame = (now: number): void => {
      const w = window.innerWidth;
      const h = window.innerHeight;
      const showing = now - this.startedAt < FireworksComponent.SHOW_MS;
      if (showing && now - this.lastLaunch >= FireworksComponent.LAUNCH_EVERY_MS) {
        this.lastLaunch = now;
        this.burst(w, h);
      }
      ctx.clearRect(0, 0, w, h);
      this.particles = this.particles.filter((p) => p.life < p.ttl);
      for (const p of this.particles) {
        p.life += 1;
        p.vy += 0.045; // gravity
        p.vx *= 0.985; // drag
        p.vy *= 0.985;
        p.px = p.x;
        p.py = p.y;
        p.x += p.vx;
        p.y += p.vy;
        const t = 1 - p.life / p.ttl;
        ctx.globalAlpha = Math.max(0, Math.sqrt(t));
        ctx.strokeStyle = p.color;
        ctx.lineWidth = p.size * (0.5 + 0.5 * t);
        ctx.beginPath();
        ctx.moveTo(p.px - p.vx * 2, p.py - p.vy * 2);
        ctx.lineTo(p.x, p.y);
        ctx.stroke();
      }
      ctx.globalAlpha = 1;
      if (!showing && this.particles.length === 0) {
        this.remove();
        return;
      }
      this.raf = requestAnimationFrame(frame);
    };
    this.raf = requestAnimationFrame(frame);
  }

  /** Launch one burst at a random point in the upper two thirds of the view. */
  private burst(w: number, h: number): void {
    const x = w * (0.12 + Math.random() * 0.76);
    const y = h * (0.08 + Math.random() * 0.55);
    const color = COLORS[this.burstIndex++ % COLORS.length];
    const count = 70 + Math.floor(Math.random() * 40);
    const speed = 3.2 + Math.random() * 2.6;
    for (let i = 0; i < count; i++) {
      const a = (Math.PI * 2 * i) / count + Math.random() * 0.12;
      const s = speed * (0.55 + Math.random() * 0.45);
      this.particles.push({
        x,
        y,
        px: x,
        py: y,
        vx: Math.cos(a) * s,
        vy: Math.sin(a) * s,
        life: 0,
        ttl: 80 + Math.floor(Math.random() * 40),
        color,
        size: 2.4 + Math.random() * 2.2,
      });
    }
  }

  private stop(): void {
    if (this.raf !== 0) cancelAnimationFrame(this.raf);
    this.raf = 0;
    this.particles = [];
  }

  /** End the show and take the canvas out of the page. */
  private remove(): void {
    this.stop();
    this.canvas().nativeElement.remove();
  }
}
