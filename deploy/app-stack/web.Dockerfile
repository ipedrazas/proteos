# syntax=docker/dockerfile:1
#
# Web image: build the React SPA, serve it with nginx, and reverse-proxy /api to
# the control plane. The SPA calls the API with relative paths (/api/...), and
# the session-cookie + CSRF model depends on the SPA and API sharing ONE origin,
# so nginx fronting both is the correct production shape — not Vite's dev proxy.
#
# Build context MUST be the repo root (needs web/ and deploy/app-stack/nginx.conf).

# --platform=$BUILDPLATFORM: the SPA build produces static files that are the
# same on every architecture, so it runs natively on the build host even when
# the image is being built for another one (an arm64 laptop building for amd64
# nodes) instead of under emulation, where npm ci and vite are painfully slow.
FROM --platform=$BUILDPLATFORM node:25-alpine AS build
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM nginx:1.27-alpine
# Installed as a TEMPLATE, not a config: the image's entrypoint runs envsubst
# over /etc/nginx/templates/*.template into /etc/nginx/conf.d/ at container
# start, which is how one nginx.conf serves both the compose stack and the
# Kubernetes app stack (deploy/kubernetes/app-stack/). The filter restricts
# substitution to PROTEOS_* names, so nginx's own $variables in the config are
# left alone. The defaults below are the compose stack's and render the config
# byte-for-byte as it was before it became a template.
COPY deploy/app-stack/nginx.conf /etc/nginx/templates/default.conf.template
ENV NGINX_ENVSUBST_FILTER='^PROTEOS_' \
    PROTEOS_DNS_RESOLVER=127.0.0.11 \
    PROTEOS_API_UPSTREAM=controlplane:8080
COPY --from=build /web/dist /usr/share/nginx/html
EXPOSE 80
