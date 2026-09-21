FROM node:22.22.2-alpine3.23@sha256:cb15fca92530d7ac113467696cf1001208dac49c3c64355fd1348c11a88ddf8f
RUN apk add --no-cache chromium=149.0.7827.53-r0
WORKDIR /app
COPY wrappers/rebrowser/package*.json ./
RUN npm ci --ignore-scripts --omit=dev --no-audit --no-fund && npm cache clean --force
COPY wrappers/rebrowser/*.mjs ./
USER node
ENV HOME=/tmp ZOMBIE_BROWSER_CONFIG=/config/browser.json
CMD ["node", "--max-old-space-size=96", "server.mjs"]
