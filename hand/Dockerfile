# agentplane hand image: a durable MCP tool server. Separate from the brain
# image on purpose — brain and hand code never mix, and each deploys on its own.
FROM node:22-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl git python3 \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY package.json /app/package.json
RUN npm install --omit=dev && npm cache clean --force

COPY src /app/src
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENV PORT=8080
ENTRYPOINT ["/entrypoint.sh"]
