# HTTPS cutover runbook — `api.supercluster.dev`

Closes threat-model **F2** (#20): today the public endpoint is plain HTTP, so
bearer tokens, prompts, and the GitHub PAT in `PUT /v1/credentials` all cross
the internet in cleartext.

Prepared already (nothing to redo):

- Cloud DNS zone **`supercluster-dev`** in this project
- **`A api.supercluster.dev → 136.68.213.85`** (the reserved `agentplane-ip`,
  already the ingress address)
- [`ingress-tls.yaml`](ingress-tls.yaml) with the real hostname, ManagedCertificate,
  and `allow-http: "false"`

## 1. Register the domain (you)

Buy `supercluster.dev`, then set the registrar's nameservers to:

```
ns-cloud-a1.googledomains.com.
ns-cloud-a2.googledomains.com.
ns-cloud-a3.googledomains.com.
ns-cloud-a4.googledomains.com.
```

(Cloud Domains does this automatically; Namecheap/Porkbun need it entered by hand.)

Wait for public resolution — this is the gate for everything below:

```bash
until dig +short api.supercluster.dev @8.8.8.8 | grep -q 136.68.213.85; do sleep 60; done
```

## 2. Apply and wait for the certificate

```bash
kubectl apply -f infra/serve/ingress-tls.yaml
# Active in ~15-60 min; Provisioning until the domain resolves publicly.
kubectl -n agentplane describe managedcertificate agentplane-cert | grep -E 'Status|Domain'
```

## 3. Verify — both halves matter

```bash
curl -fsS https://api.supercluster.dev/healthz && echo "HTTPS OK"
# Must NOT be 200 and must NOT be a 301 to https — a redirect still leaks the
# token in request one, which is why allow-http is "false".
curl -s -o /dev/null -w 'plain HTTP -> %{http_code}\n' http://api.supercluster.dev/healthz
```

## 4. Rotate every credential

Everything issued so far crossed cleartext and must be treated as compromised:

```bash
# tokens: delete the store, then each user re-runs `andromeda login`
kubectl -n agentplane delete secret agentplane-serve-tokens
# vault credentials: each user re-adds theirs (values are never recoverable)
# rotate the shared secrets too
kubectl -n agentplane delete secret agentplane-grant-key
kubectl -n agentplane create secret generic agentplane-grant-key \
  --from-literal=key=$(openssl rand -hex 32)
kubectl -n agentplane rollout restart deploy/agentplane-serve
```

Also revoke and re-issue any **GitHub PAT** that was stored via the vault, and
rotate the vendor API keys if a BYO key was ever sent over HTTP.

## 5. Point clients at HTTPS, retire cleartext

- `andromeda/ONBOARDING.md`, `andromeda/README.md`, root `README.md`
- Users: `andromeda login --url https://api.supercluster.dev`
- Delete the old host so nothing silently keeps the cleartext path:

```bash
kubectl -n agentplane delete ingress agentplane   # the nip.io ingress
```

## 6. Re-verify end to end

```bash
AGENTPLANE_URL=https://api.supercluster.dev AGENTPLANE_TOKEN=<new> \
  ./test/smoke-live.sh
```
