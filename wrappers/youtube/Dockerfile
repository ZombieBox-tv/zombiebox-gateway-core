FROM node:22.22.2-alpine3.23@sha256:cb15fca92530d7ac113467696cf1001208dac49c3c64355fd1348c11a88ddf8f
WORKDIR /app
COPY wrappers/youtube/package*.json ./
RUN npm ci --ignore-scripts --omit=dev --no-audit --no-fund && npm cache clean --force
COPY wrappers/youtube/*.mjs ./
COPY LICENSE NOTICE /usr/share/doc/zombiebox/
USER node
ENV HOST=0.0.0.0 PORT=8091 ZOMBIE_YOUTUBE_CONFIG=/config/youtube.json
EXPOSE 8091
CMD ["node", "--max-old-space-size=128", "server.mjs"]
