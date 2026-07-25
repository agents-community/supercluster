# The agentplane control-plane API (`serve`), containerized to run IN-cluster.
#
# Base is cloud-sdk (gcloud, for transcript escrow to GCS via Workload Identity);
# kubectl is added for the agent (ActorTemplate) operations. In-cluster, both
# authenticate automatically — kubectl via the pod ServiceAccount, gcloud via
# Workload Identity — so no credentials are baked in.
#
# The binary is built OUTSIDE this context (the go.mod `replace` points at a
# sibling substrate checkout that isn't in the Docker context). CI should build
# it from a proper substrate module dependency; see infra/README.md.
FROM google/cloud-sdk:slim

# kubectl isn't in Debian's default repos; fetch the official static binary.
RUN curl -fsSLo /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" \
  && chmod +x /usr/local/bin/kubectl

COPY agentplane /usr/local/bin/agentplane
RUN chmod +x /usr/local/bin/agentplane

EXPOSE 7433
ENTRYPOINT ["/usr/local/bin/agentplane", "serve", "-addr", ":7433"]
