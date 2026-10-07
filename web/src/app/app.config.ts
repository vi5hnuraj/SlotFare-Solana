import { ApplicationConfig, provideBrowserGlobalErrorListeners } from '@angular/core';
import { provideRouter, withInMemoryScrolling } from '@angular/router';
import { routes } from './app.routes';
import { provideSlotfareApi } from './core/api';

export const appConfig: ApplicationConfig = {
  providers: [
    provideBrowserGlobalErrorListeners(),
    provideRouter(
      routes,
      // Each screen is a separate destination; landing halfway down a page
      // after navigating is disorienting.
      withInMemoryScrolling({ scrollPositionRestoration: 'top' }),
    ),
    provideSlotfareApi(),
  ],
};
