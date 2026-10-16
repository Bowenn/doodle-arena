# syntax=docker/dockerfile:1
# Development image: runs the Vite dev server behind the gateway. A production build
# (static files served by Nginx or a CDN) comes with the AWS deployment in Stage 5.
FROM node:24.22.0-trixie-slim
WORKDIR /app
# Install as the unprivileged user, so Vite can write its cache into node_modules.
RUN chown node:node /app
USER node
COPY --chown=node:node package.json package-lock.json ./
RUN --mount=type=cache,target=/home/node/.npm,uid=1000,gid=1000 npm ci --no-fund --no-audit
COPY --chown=node:node . .
EXPOSE 5173
CMD ["npm", "run", "dev"]
