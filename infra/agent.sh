#!/usr/bin/env bash
# Render an agent spec for this account and print it (or create it).
#
#   source infra/config.env
#   ./infra/agent.sh agentplane/examples/starter.yaml            # print
#   ./infra/agent.sh agentplane/examples/starter.yaml --create   # POST to serve
#
# Agent specs pin an image DIGEST, and both the registry and the digest are
# per-account. This substitutes the registry and resolves the digest from the
# tag in config.env, so a spec written against one project applies to another
# without hand-editing.
set -euo pipefail
: "${IMAGE_REPO:?source infra/config.env first}"
spec="${1:?usage: agent.sh <spec.yaml> [--create]}"

# Which image does this spec use? Map it to the tag config.env names.
case "$(grep -oE 'agentplane-brain-[a-z]+' "$spec" | head -1)" in
  agentplane-brain-cc)    img=agentplane-brain-cc;    tag="${BRAIN_CC_TAG}" ;;
  agentplane-brain-codex) img=agentplane-brain-codex; tag="${BRAIN_CODEX_TAG}" ;;
  agentplane-brain-pi)    img=agentplane-brain-pi;    tag="${BRAIN_PI_TAG}" ;;
  *) echo "cannot tell which brain image $spec uses" >&2; exit 2 ;;
esac

# Resolve tag -> digest. Digest-pinning is required (a moving tag would
# invalidate the golden snapshot silently), so this must not fall back to a tag.
digest="$(gcloud container images describe "${IMAGE_REPO}/${img}:${tag}" \
  --format='value(image_summary.digest)' 2>/dev/null)"
[ -n "$digest" ] || { echo "no such image: ${IMAGE_REPO}/${img}:${tag} — run build.sh first" >&2; exit 1; }

rendered="$(sed -E "s|image: .*agentplane-brain-[a-z]+@sha256:[a-f0-9]+|image: ${IMAGE_REPO}/${img}@${digest}|" "$spec")"

if [ "${2:-}" = "--create" ]; then
  : "${AGENTPLANE_URL:?set AGENTPLANE_URL}" "${AGENTPLANE_TOKEN:?set AGENTPLANE_TOKEN}"
  printf '%s' "$rendered" | curl -fsS -X POST \
    -H "Authorization: Bearer ${AGENTPLANE_TOKEN}" -H 'Content-Type: application/yaml' \
    --data-binary @- "${AGENTPLANE_URL}/v1/agents"
  echo
else
  printf '%s\n' "$rendered"
fi
