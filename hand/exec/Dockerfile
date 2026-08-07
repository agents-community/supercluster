# agentplane executor image — the container model-written commands run in (#74).
#
# Separate from the hand image on purpose, and that separation IS the security
# property: this rootfs contains the executor and a toolchain, and nothing else.
# The gateway's source, its node_modules and anything it is configured with are
# simply absent, so there is no "hide /app" problem to solve — there is nothing
# there to hide.
#
# The toolchain lives HERE rather than in the gateway, because this is where
# commands now run. The gateway needs none of it.
FROM node:22-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl git python3 python3-pip python3-venv \
  && rm -rf /var/lib/apt/lists/*

# A virtualenv on PATH, so `pip install <pkg>` just works for the agent.
#
# Debian 12 marks the system interpreter EXTERNALLY-MANAGED (PEP 668): a bare
# `pip install` fails telling you to pass --break-system-packages. Making the
# agent discover and pass that flag is worse than giving it a venv.
#
# In /opt rather than /workspace so it is not mixed into a user's checkout, and
# it still survives suspend/resume: the actor's filesystem delta is part of the
# snapshot, so packages installed mid-session are there when the mind wakes.
ENV VIRTUAL_ENV=/opt/venv
RUN python3 -m venv "$VIRTUAL_ENV" \
  && "$VIRTUAL_ENV/bin/pip" install --no-cache-dir --upgrade pip setuptools wheel
ENV PATH="$VIRTUAL_ENV/bin:$PATH"

# git must fail rather than hang waiting for a password nobody can type.
ENV GIT_TERMINAL_PROMPT=0

WORKDIR /app
# No package.json and no npm install: the executor uses only the node stdlib.
# Every dependency is a dependency of the process running untrusted input.
COPY executor.mjs /app/executor.mjs

ENV EXEC_PORT=8081
ENV HAND_WORKDIR=/workspace

# Runs as root. Not a choice — gVisor refuses every privilege-drop route
# (spawnSync uid -> EPERM, setpriv -> setresuid EPERM, su -> cannot set
# groups), and /workspace arrives root:root 0700 so a non-root process cannot
# enter it. EXEC_UID stays 0 until Substrate can create the DurableDir owned by
# a non-root uid; see #74.
#
# The blast radius is this container: no credentials live here, and the gateway
# is a separate rootfs. An agent rewriting this image affects its own session
# and nothing else.
ENV EXEC_UID=0
ENV EXEC_HOME=/root

ENTRYPOINT ["node", "/app/executor.mjs"]
