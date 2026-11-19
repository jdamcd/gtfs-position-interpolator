# syntax=docker/dockerfile:1
FROM node:24-alpine AS build
WORKDIR /ui
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
# The Caddyfile proxies the feed on the UI's own origin.
ENV VITE_POSITIONS_URL=/positions
RUN --mount=type=secret,id=VITE_MAPBOX_TOKEN,env=VITE_MAPBOX_TOKEN npm run build

FROM caddy:2-alpine
COPY Caddyfile /etc/caddy/Caddyfile
COPY --from=build /ui/dist /srv
