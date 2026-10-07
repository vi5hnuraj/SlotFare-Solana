import { Component, input } from '@angular/core';
import { IconComponent, IconName } from './icon.component';

/**
 * The design's .state-block for a list that has nothing in it yet: a mark
 * with an icon when one is given, a title, and an optional detail line.
 */
@Component({
  selector: 'slf-empty-block',
  imports: [IconComponent],
  template: `
    <div class="state-block">
      @if (icon()) {
        <div class="mark"><slf-icon [name]="icon()!" /></div>
      }
      <h4>{{ title() }}</h4>
      @if (detail()) {
        <p>{{ detail() }}</p>
      }
    </div>
  `,
})
export class EmptyBlockComponent {
  readonly title = input.required<string>();
  readonly detail = input('');
  readonly icon = input<IconName | null>(null);
}
