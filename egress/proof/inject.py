# mitmproxy addon: secretless credential injection at the egress boundary.
#
# The hand makes UNCREDENTIALED outbound calls. This addon terminates TLS
# (mitmproxy's CA, which the hand trusts), matches the destination host against
# a per-session policy, injects the credential, and re-originates to upstream.
# The secret exists only here — never in the sandbox that ran the request.
#
# Deny-by-default: any host not in the policy is refused with 403.
#
# In production `policy.json` is not a static file — serve renders it per
# session from the GCP Secret Manager vault (destination -> secret mapping),
# exactly as it configures the hand's upstreams today.

import base64
import json
import os

from mitmproxy import http

POLICY_PATH = os.environ.get("EGRESS_POLICY", "/policy.json")


def _load_policy():
    with open(POLICY_PATH) as f:
        return json.load(f).get("allow", {})


POLICY = _load_policy()


def request(flow: http.HTTPFlow) -> None:
    host = flow.request.pretty_host
    rule = POLICY.get(host)

    if rule is None:
        flow.response = http.Response.make(
            403, b"egress denied by policy\n", {"Content-Type": "text/plain"}
        )
        print(f"DENY {host}")
        return

    kind = rule.get("type", "none")

    if kind == "bearer":
        flow.request.headers["Authorization"] = "Bearer " + rule["token"]
        print(f"INJECT bearer -> {host}")
    elif kind == "basic":
        raw = f"{rule['username']}:{rule['token']}".encode()
        flow.request.headers["Authorization"] = "Basic " + base64.b64encode(raw).decode()
        print(f"INJECT basic -> {host}")
    else:
        # allowlisted, no credential (e.g. public github.com anonymous clone)
        print(f"ALLOW {host} (no inject)")
