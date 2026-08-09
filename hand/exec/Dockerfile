# agentplane hand ZONE image — the gVisor sandbox a session's commands run in.
#
# Commands are launched from OUTSIDE the actor via the control plane's ExecActor
# (`runsc exec`); the runtime lives in the broker, NOT here. So this image carries
# ONLY the toolchain a command might use and the /workspace mount — no server, no
# code of ours. `ls /` inside a command shows the zone and nothing else. The
# container's only job is to stay alive so `runsc exec` has something to enter.
FROM node:22-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl git python3 python3-pip python3-venv \
  && rm -rf /var/lib/apt/lists/*

# A virtualenv on PATH so `pip install <pkg>` just works for the agent. Debian 12
# marks the system interpreter EXTERNALLY-MANAGED (PEP 668); a venv avoids making
# the agent discover --break-system-packages. In /opt so it is not mixed into a
# user's checkout, and it survives suspend/resume in the snapshot's fs delta.
ENV VIRTUAL_ENV=/opt/venv
RUN python3 -m venv "$VIRTUAL_ENV" \
  && "$VIRTUAL_ENV/bin/pip" install --no-cache-dir --upgrade pip setuptools wheel
ENV PATH="$VIRTUAL_ENV/bin:$PATH"

# git must fail rather than hang waiting for a password nobody can type.
ENV GIT_TERMINAL_PROMPT=0
ENV HAND_WORKDIR=/workspace

# Keep-alive: PID 1 does nothing but stay running so `runsc exec` can enter the
# sandbox. There is no executor.mjs, no /app, no server here — the runtime is the
# broker, outside gVisor. This is the whole point: a command's `ls /` shows the
# workspace + toolchain and none of our code.
ENTRYPOINT ["sleep", "infinity"]
