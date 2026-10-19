# syntax=docker/dockerfile:1
# Two images from one file:
#   dev     the Vite dev server behind the gateway (the local stack: compose builds this target)
#   static  the production build, static files served by Nginx (AWS, ADR 0003; the default)
FROM node:24.22.0-trixie-slim AS deps
WORKDIR /app
# Install as the unprivileged user, so Vite can write its cache into node_modules.
RUN chown node:node /app
USER node
COPY --chown=node:node package.json package-lock.json ./
RUN --mount=type=cache,target=/home/node/.npm,uid=1000,gid=1000 npm ci --no-fund --no-audit
COPY --chown=node:node . .

FROM deps AS dev
EXPOSE 5173
CMD ["npm", "run", "dev"]

FROM deps AS build
RUN npm run build

FROM nginx:1.30.6 AS static
RUN rm -f /etc/nginx/conf.d/default.conf
COPY static.conf /etc/nginx/conf.d/web.conf
COPY --from=build /app/dist /usr/share/nginx/html
RUN nginx -t
EXPOSE 8080
