import { bootstrapApplication } from '@angular/platform-browser';
import { appConfig } from './app/app.config';
import { App } from './app/app';
import {
  API_BASE_URL,
  NETWORK,
  loadRuntimeConfig,
  resolveApiBaseUrl,
  resolveNetwork,
} from './app/core/runtime-config';

// The container writes config.json from the API_BASE_URL env var at boot;
// fetching it before bootstrap lets one image serve any environment. Any
// failure (404 in dev, network, malformed JSON) resolves to null and the app
// boots on the compiled environment.apiUrl fallback.
loadRuntimeConfig()
  .then((runtime) =>
    bootstrapApplication(App, {
      ...appConfig,
      providers: [
        ...appConfig.providers,
        { provide: API_BASE_URL, useValue: resolveApiBaseUrl(runtime) },
        { provide: NETWORK, useValue: resolveNetwork(runtime) },
      ],
    }),
  )
  .catch((err) => console.error(err));
