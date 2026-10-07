import { Component, input } from '@angular/core';

export type IconName =
  | 'sun' | 'moon' | 'copy' | 'plus' | 'check' | 'close' | 'external'
  | 'search' | 'shield' | 'info' | 'chevron'
  | 'grid' | 'plus-circle' | 'route' | 'key' | 'bars' | 'menu' | 'cube';

/**
 * One inline SVG per name. Kept in a single component so stroke weights and
 * viewBoxes stay consistent, and so a template never carries 8 lines of
 * path data inline.
 */
@Component({
  selector: 'slf-icon',
  template: `
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor"
         [attr.stroke-width]="width()" stroke-linecap="round"
         stroke-linejoin="round" aria-hidden="true">
      @switch (name()) {
        @case ('sun') {
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2m0 16v2M2 12h2m16 0h2M4.9 4.9l1.5 1.5m11.2 11.2 1.5 1.5M19.1 4.9l-1.5 1.5M6.4 17.6l-1.5 1.5" />
        }
        @case ('moon') {
          <path d="M20 14.5A8.5 8.5 0 1 1 9.5 4a7 7 0 0 0 10.5 10.5Z" />
        }
        @case ('copy') {
          <rect x="9" y="9" width="12" height="12" rx="2" />
          <path d="M5 15V5a2 2 0 0 1 2-2h10" />
        }
        @case ('plus') { <path d="M12 5v14M5 12h14" /> }
        @case ('check') { <path d="m4 12 5.5 5.5L20 7" /> }
        @case ('close') { <path d="m6 6 12 12M18 6 6 18" /> }
        @case ('external') { <path d="M7 17 17 7M9 7h8v8" /> }
        @case ('search') {
          <circle cx="11" cy="11" r="7" /><path d="m16.5 16.5 4 4" />
        }
        @case ('shield') {
          <path d="M12 3 4 6v6c0 4.4 3.2 8.1 8 9 4.8-.9 8-4.6 8-9V6l-8-3Z" />
          <path d="M12 10v4m0-6.5v.01" />
        }
        @case ('info') {
          <circle cx="12" cy="12" r="9" /><path d="M12 11v5m0-8.5v.01" />
        }
        @case ('chevron') { <path d="m9 6 6 6-6 6" /> }
        @case ('grid') {
          <rect x="3" y="3" width="7" height="9" rx="1.5" />
          <rect x="14" y="3" width="7" height="5" rx="1.5" />
          <rect x="14" y="12" width="7" height="9" rx="1.5" />
          <rect x="3" y="16" width="7" height="5" rx="1.5" />
        }
        @case ('plus-circle') {
          <circle cx="12" cy="12" r="9" /><path d="M12 8v8M8 12h8" />
        }
        @case ('route') {
          <circle cx="6" cy="6" r="2.5" /><circle cx="18" cy="18" r="2.5" />
          <path d="M6 8.5v5a4 4 0 0 0 4 4h5.5" />
        }
        @case ('key') {
          <circle cx="8" cy="15" r="4" />
          <path d="m11 12 9-9M17 6l2.5 2.5M14.5 8.5 17 11" />
        }
        @case ('bars') { <path d="M4 19V9m5 10V5m5 14v-7m5 7V8" /> }
        @case ('menu') { <path d="M4 7h16M4 12h16M4 17h16" /> }
        @case ('cube') {
          <path d="M12 2 3 7v10l9 5 9-5V7l-9-5Z" />
          <path d="M3 7l9 5 9-5M12 12v10" />
        }
      }
    </svg>
  `,
})
export class IconComponent {
  readonly name = input.required<IconName>();
  readonly width = input(1.7);
}
