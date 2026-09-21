FROM node:22.22.2-alpine3.23@sha256:cb15fca92530d7ac113467696cf1001208dac49c3c64355fd1348c11a88ddf8f
WORKDIR /app
COPY wrappers/youtube-receiver/package*.json ./
RUN npm ci --ignore-scripts --omit=dev --no-audit --no-fund && npm cache clean --force
COPY wrappers/youtube-receiver/*.mjs ./
USER node
ENV ZOMBIE_YOUTUBE_RECEIVER_CONFIG=/config/receiver.json
EXPOSE 8095 8096
CMD ["node", "--max-old-space-size=192", "server.mjs"]
