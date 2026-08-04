# agentplane hand image: a durable MCP tool server. Separate from the brain
# image on purpose — brain and hand code never mix, and each deploys on its own.
FROM node:22-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl git python3 python3-pip python3-venv \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY package.json /app/package.json
# A virtualenv on PATH, so `pip install <pkg>` just works for the agent.
#
# Debian 12 marks the system interpreter EXTERNALLY-MANAGED (PEP 668): a bare
# `pip install` fails telling you to pass --break-system-packages. Making the
# agent discover and pass that flag is worse than giving it a venv — and
# breaking system packages in a sandbox that also runs our own tooling is a way
# to make the hand itself unbootable.
#
# It lives in /opt rather than /workspace so it is not mixed into a user's
# checkout, and it still survives suspend/resume: the actor's filesystem delta
# is part of the snapshot, so packages installed mid-session are there when the
# mind wakes.
ENV VIRTUAL_ENV=/opt/venv
RUN python3 -m venv "$VIRTUAL_ENV" \
  && "$VIRTUAL_ENV/bin/pip" install --no-cache-dir --upgrade pip setuptools wheel
ENV PATH="$VIRTUAL_ENV/bin:$PATH"

RUN npm install --omit=dev && npm cache clean --force

COPY src /app/src
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENV PORT=8080
ENTRYPOINT ["/entrypoint.sh"]
