# infra/terraform — cloud resources (planned)

The Kubernetes manifests in `../serve` and `../gateway` assume the cloud
substrate already exists. This directory will hold the **Terraform** that
provisions it, **per provider** (the roadmap: CI/CD per provider).

Planned modules:

```
terraform/
  gcp/                 # first target (the cluster runs here today)
    cluster.tf         # GKE cluster + node pools (gVisor-capable)
    network.tf         # global static IP, DNS zone + A record for api.<domain>
    storage.tf         # snapshot bucket with CMEK (the BYO-key at-rest fix)
    kms.tf             # KMS keyring/key for CMEK
    iam.tf             # Workload-Identity GSA for serve (storage.objectAdmin)
    secrets.tf         # the serve bearer token
  modules/             # provider-agnostic building blocks
```

Until then, provision by hand — see `../README.md` for the exact `gcloud`
commands (static IP, CMEK bucket, Workload Identity binding).

Note on the CMEK bucket: this is where the BYO-key at-rest concern is closed —
an actor's checkpoint contains the session's ephemeral key in memory, so the
snapshot bucket must be encrypted with a customer-managed key.
