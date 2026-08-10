#!/usr/bin/env bash
# Render every manifest with the values in config.env.
#
#   source infra/config.env && ./infra/render.sh | kubectl apply -f -
#
# Renders to stdout so nothing half-applied is ever written to disk, and so the
# output can be diffed before it is applied:
#
#   source infra/config.env && ./infra/render.sh | kubectl diff -f - || true
set -euo pipefail

: "${PROJECT_ID:?source infra/config.env first}"
: "${SNAPSHOT_BUCKET:?source infra/config.env first}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Only these are substituted. envsubst with no list would also eat $() and any
# other shell-looking text inside the manifests.
vars='$PROJECT_ID $PROJECT_NUMBER $SNAPSHOT_BUCKET $IMAGE_REPO $SERVE_TAG $EXEC_TAG
$GITPROXY_TAG $BROKER_TAG $BRAIN_CC_TAG $HAND_TEMPLATE $INGRESS_IP $INGRESS_HOST $OTEL_ENDPOINT
$CLUSTER_NAME $CLUSTER_LOCATION $REGION
$BRAIN_CC_IMAGE $BRAIN_PI_IMAGE $BRAIN_CODEX_IMAGE'

first=1
for f in "$root"/infra/serve/*.yaml "$root"/gitproxy/deploy.yaml "$root"/broker/deploy.yaml; do
  case "$(basename "$f")" in
    # networkpolicy is environment-independent; ingress-tls is applied by hand
    # during the HTTPS cutover, not as part of a normal deploy.
    networkpolicy.yaml|ingress-tls.yaml) continue ;;
    # not a kubernetes manifest — built with `gcloud builds submit`, see build.sh
    cloudbuild.yaml) continue ;;
    # the email allowlist holds ACCOUNT-SPECIFIC data (real testers/admins) and
    # the committed file is only a placeholder template — applying it here would
    # WIPE the live allowlist. Maintain it out-of-band, e.g.:
    #   kubectl -n agentplane apply -f your-real-allowed-emails.yaml
    allowed-emails.configmap.yaml) continue ;;
  esac
  # No separator before the first document: a leading `---` makes an empty one,
  # which kubectl rejects with "apiVersion not set".
  [ "$first" -eq 1 ] || echo "---"
  first=0
  envsubst "$vars" < "$f"
done
