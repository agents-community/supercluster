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

build() { # dir, config, tagvar
  local dir="$1" cfg="$2" tag="$3"
  echo "==> $dir ($cfg) → ${IMAGE_REPO}:${!tag}"
  ( cd "$root/$dir" && envsubst '$IMAGE_REPO $SERVE_TAG $HAND_TAG $GITPROXY_TAG $BRAIN_CC_TAG $BRAIN_CODEX_TAG $BRAIN_PI_TAG' \
      < "$cfg" > /tmp/cloudbuild.rendered.yaml \
    && gcloud builds submit --config /tmp/cloudbuild.rendered.yaml . )
}

case "$what" in
  serve)    build infra/serve       cloudbuild.yaml     SERVE_TAG ;;
  hand)     build hand              cloudbuild.yaml     HAND_TAG ;;
  gitproxy) build gitproxy          cloudbuild.yaml     GITPROXY_TAG ;;
  brain-cc) build agentplane/brain  cloudbuild-cc.yaml    BRAIN_CC_TAG ;;
  brain-codex) build agentplane/brain cloudbuild-codex.yaml BRAIN_CODEX_TAG ;;
  brain-pi) build agentplane/brain  cloudbuild-pi.yaml      BRAIN_PI_TAG ;;
  all)
    # serve first: the dispatcher shares its image, so a half-built set leaves
    # the two on different versions — which is how auto-sleep died silently once.
    build infra/serve       cloudbuild.yaml     SERVE_TAG
    build hand              cloudbuild.yaml     HAND_TAG
    build gitproxy          cloudbuild.yaml     GITPROXY_TAG
    build agentplane/brain  cloudbuild-cc.yaml  BRAIN_CC_TAG
    ;;
  *) echo "usage: $0 [serve|hand|gitproxy|brain-cc|brain-codex|brain-pi|all]" >&2; exit 2 ;;
esac
