import { Component, inject } from '@angular/core';
import { RouterLink } from '@angular/router';
import { ThemeService } from '../core/theme';
import { IconComponent } from '../ui/icon.component';
import { FireworksComponent } from '../ui/fireworks.component';

/** One phase on the road to mainnet, in build order. */
export interface Phase {
  title: string;
  makesTrue: string;
  doneWhen: string;
}

/** The four phases, mirrored from the project's development plan
 *  (Doc = Truth: when this list and that page disagree, one of them is a bug). */
export const PHASES: readonly Phase[] = [
  {
    title: 'Lease escrow on chain',
    makesTrue:
      'Credits are encumbered on chain for the first time. A lease holds a rolling deposit, accrues per epoch, cancels on exhaustion, and applies the tier’s penalty schedule.',
    doneWhen: 'A lease can be opened, funded, accrued and closed on a public testnet from a script.',
  },
  {
    title: 'Provider image and agent',
    makesTrue:
      'A provider flashes a Slotfare NixOS image. Its agent registers the machine, watches the lease contract, opens the tunnel for the lessee’s key, and reboots clean when the lease ends.',
    doneWhen: 'One machine, listed from the image with no manual steps, serves one lease end to end.',
  },
  {
    title: 'Prober quorum',
    makesTrue:
      'Liveness evidence comes from probers, not the provider. Accrual pays only against their evidence, and a bonded dispute invalidates evidence under deterministic rules.',
    doneWhen: 'A provider that goes dark stops being paid, without anyone deciding it.',
  },
  {
    title: 'Lease marketplace',
    makesTrue:
      'A new console, built for this product. Browse providers with tier and history, lease with escrow, generate your keys in the browser, connect.',
    doneWhen: 'A stranger leases a stranger’s machine and reaches it.',
  },
];

/**
 * The public roadmap: what is live on testnet, what the product is, the
 * four phases that build it on testnet, and the conditions that gate
 * mainnet. No wallet needed.
 */
@Component({
  selector: 'slf-roadmap',
  imports: [RouterLink, IconComponent, FireworksComponent],
  templateUrl: './roadmap.component.html',
})
export class RoadmapComponent {
  protected readonly theme = inject(ThemeService);
  protected readonly phases = PHASES;
}
