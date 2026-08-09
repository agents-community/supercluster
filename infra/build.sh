#!/usr/bin/env bash
# Build and push every image, into whatever project config.env names.
#
#   source infra/config.env && ./infra/build.sh [serve|hand|gitproxy|brain-cc|all]
#
# cloudbuild.yaml files carry ${IMAGE_REPO}/${TAG} placeholders rather than a
# literal registry, so the same file builds into any account.
set -euo pipefail
: "${IMAGE_REPO:?source infra/config.env first}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
what="${1:-all}"

# NEVER reuse a tag. Deployments run imagePullPolicy: IfNotPresent, so pushing
# new code to an existing tag changes nothing: the manifest is identical, so no
# rollout happens, and even `rollout restart` reuses the node's cached layer.
# The symptom is a deploy that reports success while the old binary keeps
# running — which cost an hour of debugging a "stale atenet" that was really a
# stale pod.
build() { # dir, config, tagvar
  local dir="$1" cfg="$2" tag="$3"
  echo "==> $dir ($cfg) → ${IMAGE_REPO}:${!tag}"
  ( cd "$root/$dir" && envsubst '$IMAGE_REPO $SERVE_TAG $EXEC_TAG $GITPROXY_TAG $BROKER_TAG $BRAIN_CC_TAG $BRAIN_CODEX_TAG $BRAIN_PI_TAG' \
      < "$cfg" > /tmp/cloudbuild.rendered.yaml \
    && gcloud builds submit --config /tmp/cloudbuild.rendered.yaml . )
}

# The serve image COPYs a prebuilt binary: its Docker context is infra/serve,
# and the go.mod `replace` points at a sibling substrate checkout that cannot be
# in that context. So the binary must be compiled here, first.
#
# This is not a convenience. Without it `build.sh serve` silently packages
# whatever binary happens to be sitting in infra/serve — which is how two
# releases shipped a build from the previous day while every check looked
# green, because /readyz answers the same on old and new code.
build_serve_binary() {
  echo "==> compiling serve binary (infra/serve/agentplane)"
  ( cd "$root/agentplane" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -o "$root/infra/serve/agentplane" ./cmd/agentplane )
}

case "$what" in
  serve)    build_serve_binary; build infra/serve cloudbuild.yaml SERVE_TAG ;;
  exec)     build hand/exec         cloudbuild.yaml     EXEC_TAG ;;
  gitproxy) build gitproxy          cloudbuild.yaml     GITPROXY_TAG ;;
  broker)   build broker            cloudbuild.yaml     BROKER_TAG ;;
  brain-cc) build agentplane/brain  cloudbuild-cc.yaml    BRAIN_CC_TAG ;;
  brain-codex) build agentplane/brain cloudbuild-codex.yaml BRAIN_CODEX_TAG ;;
  brain-pi) build agentplane/brain  cloudbuild-pi.yaml      BRAIN_PI_TAG ;;
  all)
    # serve first: the dispatcher shares its image, so a half-built set leaves
    # the two on different versions — which is how auto-sleep died silently once.
    build_serve_binary
    build infra/serve       cloudbuild.yaml     SERVE_TAG
    build gitproxy          cloudbuild.yaml     GITPROXY_TAG
    build agentplane/brain  cloudbuild-cc.yaml  BRAIN_CC_TAG
    ;;
  *) echo "usage: $0 [serve|hand|exec|gitproxy|broker|brain-cc|brain-codex|brain-pi|all]" >&2; exit 2 ;;
esac
