# SlotfareConsole

This project was generated using [Angular CLI](https://github.com/angular/angular-cli) version 22.1.5.

## Development server

To start a local development server, run:

```bash
ng serve
```

Once the server is running, open your browser and navigate to `http://localhost:4200/`. The application will automatically reload whenever you modify any of the source files.

## Code scaffolding

Angular CLI includes powerful code scaffolding tools. To generate a new component, run:

```bash
ng generate component component-name
```

For a complete list of available schematics (such as `components`, `directives`, or `pipes`), run:

```bash
ng generate --help
```

## Building

To build the project run:

```bash
ng build
```

This will compile your project and store the build artifacts in the `dist/` directory. By default, the production build optimizes your application for performance and speed.

## Running unit tests

To execute unit tests with the [Vitest](https://vitest.dev/) test runner, use the following command:

```bash
ng test
```

## Container image

The console ships as a multi-stage image, `svrnty/slotfare-console` (private
Docker Hub). One image serves any environment: the API base URL is runtime
configuration, not a build-time constant.

### Runtime API base URL

- The container's entrypoint writes `/usr/share/nginx/html/config.json`
  from the `API_BASE_URL` env var before nginx starts (`docker/runtime-config.sh`).
- The app fetches `config.json` (cache: no-store) at boot, before bootstrap,
  and uses its `apiUrl` as the API base when it is a non-empty string. An
  unset or empty `API_BASE_URL` produces `{"apiUrl":""}` - same-origin
  relative URLs, exactly the dev-server behavior. A missing or malformed
  config falls back to the compiled `environment.apiUrl`; the app never
  white-screens on config failure.
- Probe: `GET /health` returns 200 `ok` for liveness/readiness.
- Caching: hashed assets are `public, max-age=31536000, immutable`;
  `index.html` and `config.json` are no-cache; all other routes fall back to
  `index.html` (SPA).

### Build and tag

The target cluster is arm64, so arm64 is the required platform. Tag with
the git short SHA (or `latest` for the testnet image):

```bash
docker buildx build --platform linux/arm64 \
  -t svrnty/slotfare-console:$(git rev-parse --short HEAD) --load .
```

Multi-arch (amd64 + arm64) requires pushing, since `--load` accepts only one
platform:

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t svrnty/slotfare-console:<tag> --push .
```

Pushing is out of scope for this repo; the operator supplies Docker Hub
credentials (`docker login`) and pushes or pulls from there.

## CI/CD (Gitea Actions)

The repo lives on Gitea, and Gitea Actions only loads
GitHub-Actions-syntax YAML from `.gitea/workflows/` at the **repository
root**. The console's workflows live there (alongside the Go and program
jobs), not under `web/` - a copy under `web/.gitea/` was never picked up by
Gitea. The runner assumptions live in the marked comment block at the top
of each workflow - edit them there.

### `ci.yml`

Runs on every push and pull request as one of three jobs; the other two
build and test the Go daemon and the on-chain program. The console job
checks out the code, restores the npm package cache keyed by the lockfile
hash (`web/package-lock.json`), installs with `make web-install` (`npm ci`),
then runs this directory's own quality gates: `web/scripts/ng-gate.sh
build` and `web/scripts/ng-gate.sh test --watch=false`. Unit tests are
headless Vitest-in-jsdom; no browser, Chromium or xvfb is needed on the
runner. The repo defines no lint gate today, so this workflow does not add
a format/lint step. From the repository root, `make check` runs the same
gates locally.

### `release.yml`

Runs when a release event is published in Gitea (a release, not a bare
tag push). It builds `linux/arm64` via docker buildx from the Dockerfile in
this directory and pushes `${REGISTRY_URL}/slotfare-console:${tag}` and
`${REGISTRY_URL}/slotfare-console:latest`. The registry and credentials come
from Gitea organisation secrets; no registry host or credential is written
into the workflow.

### Required Gitea secrets

- `REGISTRY_URL` - image prefix: the Docker Hub organisation (e.g. `svrnty`),
  or a private registry host plus namespace.
- `REGISTRY_USERNAME` - registry username.
- `REGISTRY_PASSWORD` - registry password/token.

### Cutting a release

Tag, push, then publish a Gitea release for that tag - publishing the
release is what triggers the workflow, and the release tag becomes the
image tag:

```bash
git tag v0.4.0 && git push origin v0.4.0
# then create a release for v0.4.0 in the Gitea UI or `gitea release`
```

## Running end-to-end tests

For end-to-end (e2e) testing, run:

```bash
ng e2e
```

Angular CLI does not come with an end-to-end testing framework by default. You can choose one that suits your needs.

## Additional Resources

For more information on using the Angular CLI, including detailed command references, visit the [Angular CLI Overview and Command Reference](https://angular.dev/tools/cli) page.
