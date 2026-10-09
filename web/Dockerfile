# slotfare-console container image.
#
# One image serves any environment: the API base URL is runtime
# configuration (API_BASE_URL env var -> config.json written at boot), not a
# build-time constant. See README.md, "Container image".
#
# Build (target cluster is arm64):
#   docker buildx build --platform linux/arm64 -t svrnty/slotfare-console:<tag> --load .

# ---- Build stage ---------------------------------------------------------
# Pin the build to the host architecture so cross-building for arm64 never
# runs a foreign-arch node binary.
FROM --platform=$BUILDPLATFORM node:22-alpine AS build
WORKDIR /app

# Dependencies first so this layer is cached until the lockfile changes.
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY angular.json tsconfig.json tsconfig.app.json tsconfig.spec.json ./
COPY src ./src
COPY public ./public
RUN npx ng build --configuration production

# ---- Runtime stage -------------------------------------------------------
# The slim variant drops the extra modules (njs, xslt, image filter, geoip)
# this config never loads, and with them most of the scanned CVEs; apk upgrade
# takes the base's pending Alpine security fixes.
FROM nginx:1-alpine-slim
RUN apk upgrade --no-cache

COPY docker/nginx.conf /etc/nginx/conf.d/default.conf
COPY docker/runtime-config.sh /docker-entrypoint.d/40-runtime-config.sh
# Do not rely on the git exec bit surviving COPY.
RUN chmod +x /docker-entrypoint.d/40-runtime-config.sh

COPY --from=build /app/dist/slotfare-console/browser/ /usr/share/nginx/html/

# No USER switch: the entrypoint script writes config.json into the html
# root, and k8s keeps the container's default user.
EXPOSE 80
