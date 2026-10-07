import { Routes } from '@angular/router';
import { inject } from '@angular/core';
import { Router } from '@angular/router';
import { ConsoleStore } from './core/console-store';

/** Everything behind the shell needs a connected wallet whose sign-in has
 *  been verified. An address alone proves nothing; the signature does. */
const connected = () => {
  const store = inject(ConsoleStore);
  const router = inject(Router);
  return store.signedIn() ? true : router.createUrlTree(['/connect']);
};

export const routes: Routes = [
  {
    path: 'connect',
    loadComponent: () =>
      import('./screens/connect.component').then((m) => m.ConnectComponent),
  },
  // The roadmap is public reading: it sits outside the signed-in shell.
  {
    path: 'roadmap',
    loadComponent: () =>
      import('./screens/roadmap.component').then((m) => m.RoadmapComponent),
  },
  {
    path: '',
    canActivate: [connected],
    loadComponent: () =>
      import('./screens/shell.component').then((m) => m.ShellComponent),
    children: [
      {
        path: 'overview',
        loadComponent: () =>
          import('./screens/overview.component').then((m) => m.OverviewComponent),
      },
      {
        path: 'add',
        loadComponent: () =>
          import('./screens/add-credits.component').then((m) => m.AddCreditsComponent),
      },
      {
        path: 'funding',
        loadComponent: () =>
          import('./screens/purchases.component').then((m) => m.PurchasesComponent),
      },
      {
        path: 'funding/:id',
        loadComponent: () =>
          import('./screens/funding-trace.component').then((m) => m.FundingTraceComponent),
      },
      {
        path: 'trade/:id',
        loadComponent: () =>
          import('./screens/trade-detail.component').then((m) => m.TradeDetailComponent),
      },
      {
        path: 'sell',
        loadComponent: () =>
          import('./screens/sell.component').then((m) => m.SellComponent),
      },
      {
        path: 'market',
        loadComponent: () =>
          import('./screens/market.component').then((m) => m.MarketComponent),
      },
      {
        path: 'keys',
        loadComponent: () =>
          import('./screens/api-keys.component').then((m) => m.ApiKeysComponent),
      },
      {
        path: 'usage',
        loadComponent: () =>
          import('./screens/usage.component').then((m) => m.UsageComponent),
      },
      {
        path: 'models',
        loadComponent: () =>
          import('./screens/models.component').then((m) => m.ModelsComponent),
      },
      {
        path: 'models/:id',
        loadComponent: () =>
          import('./screens/model-detail.component').then((m) => m.ModelDetailComponent),
      },
      {
        path: 'endpoint',
        redirectTo: 'models',
      },
      { path: '', pathMatch: 'full', redirectTo: 'overview' },
    ],
  },
  { path: '**', redirectTo: '' },
];
