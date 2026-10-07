import { Service, effect, signal } from '@angular/core';

export type Theme = 'light' | 'dark';

const KEY = 'slotfare.theme';

@Service()
export class ThemeService {
  readonly theme = signal<Theme>(initial());

  constructor() {
    effect(() => {
      const t = this.theme();
      document.documentElement.setAttribute('data-theme', t);
      try {
        localStorage.setItem(KEY, t);
      } catch {
        /* private browsing, or storage disabled: the theme still applies */
      }
    });
  }

  toggle(): void {
    this.theme.update((t) => (t === 'dark' ? 'light' : 'dark'));
  }
}

function initial(): Theme {
  try {
    const stored = localStorage.getItem(KEY);
    if (stored === 'light' || stored === 'dark') return stored;
  } catch {
    /* fall through to the OS preference */
  }
  return globalThis.matchMedia?.('(prefers-color-scheme: dark)').matches
    ? 'dark'
    : 'light';
}
