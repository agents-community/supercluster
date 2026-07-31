// AgentPlane HTTP client — the ONLY thing andromeda knows about the backend.
//
// Speaks the control-plane API (`agentplane serve`): plain JSON over HTTP plus
// SSE for the live turn stream. No kubectl, no gRPC, no cluster access — a
// URL and a bearer token are the entire contract, which is what makes this
// TUI distributable: hand someone both and they're talking to your cluster.

import http from "node:http";
import https from "node:https";

export class Client {
  constructor({ url, token }) {
    this.base = url.replace(/\/+$/, "");
    this.token = token;
  }

  headers(extra = {}) {
    return {
      ...(this.token ? { Authorization: `Bearer ${this.token}` } : {}),
      "Content-Type": "application/json",
      ...extra,
    };
  }

  async json(method, path, body) {
    const res = await fetch(this.base + path, {
      method,
      headers: this.headers(),
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    });
    const text = await res.text();
    let data;
    try { data = text ? JSON.parse(text) : {}; } catch { data = { raw: text }; }
    if (!res.ok) {
      const msg = data?.error?.message || data?.error || `HTTP ${res.status}`;
      const err = new Error(msg);
      err.status = res.status;
      throw err;
    }
    return data;
  }

  health() { return this.json("GET", "/healthz"); }
  // Self-service login: exchange an allowlisted email for a token. No auth —
  // this is how you get your first token. Returns { user, token, reissued }.
  access(email) { return this.json("POST", "/v1/access", { email }); }

  // Credential vault — store a secret (e.g. a git PAT) once; an agent that
  // lists it in `credentials:` has its hand pull it for the task. Write-only:
  // values are never returned, only names.
  listCredentials() { return this.json("GET", "/v1/credentials").then((d) => d.credentials ?? []); }
  putCredential(name, payload) { return this.json("PUT", `/v1/credentials/${encodeURIComponent(name)}`, payload); }
  deleteCredential(name) { return this.json("DELETE", `/v1/credentials/${encodeURIComponent(name)}`); }

  // Agents — create one from an AgentSpec (YAML). Returns { name, phase, note }.
  async createAgent(specYaml) {
    const res = await fetch(this.base + "/v1/agents", {
      method: "POST",
      headers: this.headers({ "Content-Type": "application/yaml" }),
      body: specYaml,
    });
    const text = await res.text();
    let d; try { d = text ? JSON.parse(text) : {}; } catch { d = { raw: text }; }
    if (!res.ok) { const e = new Error(d?.error?.message || d?.error || `HTTP ${res.status}`); e.status = res.status; throw e; }
    return d;
  }
  agents() { return this.json("GET", "/v1/agents").then((d) => d.agents ?? []); }
  sessions() { return this.json("GET", "/v1/sessions").then((d) => d.sessions ?? []); }
  // apiKey (optional) is the user's BYO vendor key — sent once over TLS, held
  // only in the actor's memory for this session, never stored.
  createSession(agent, apiKey) {
    return this.json("POST", "/v1/sessions", apiKey ? { agent, apiKey } : { agent });
  }
  setKey(id, apiKey) { return this.json("PUT", `/v1/sessions/${id}/key`, { apiKey }); }
  session(id) { return this.json("GET", `/v1/sessions/${id}`); }
  suspend(id) { return this.json("POST", `/v1/sessions/${id}/suspend`); }
  events(id, since) {
    const q = since ? `?since=${encodeURIComponent(since)}` : "";
    return this.json("GET", `/v1/sessions/${id}/message${q}`).then((d) => d.events ?? []);
  }

  // send retries 5xx: a message to a sleeping mind races its wake (server
  // retries too, but a second layer costs nothing and covers proxy blips).
  async send(id, message, onRetry) {
    let lastErr;
    for (let attempt = 1; attempt <= 3; attempt++) {
      try {
        return await this.json("POST", `/v1/sessions/${id}/message`, { message });
      } catch (e) {
        lastErr = e;
        if (!e.status || e.status < 500) throw e;
        onRetry?.(attempt);
        await new Promise((r) => setTimeout(r, 4000));
      }
    }
    throw lastErr;
  }

  /**
   * Stream SSE events after `since`. Calls onEvent(ev) per event; resolves at
   * turn end (status_idle / session.error). Returns the last event id.
   * Uses node's http module — its streaming is dependable across versions.
   */
  streamTurn(id, since, onEvent, { timeoutMs = 7 * 60 * 1000 } = {}) {
    const u = new URL(`${this.base}/v1/sessions/${id}/message/stream`);
    if (since) u.searchParams.set("since", since);
    const mod = u.protocol === "https:" ? https : http;
    return new Promise((resolve, reject) => {
      let cursor = since || "";
      const req = mod.request(u, { headers: this.headers({ Accept: "text/event-stream" }) }, (res) => {
        if (res.statusCode !== 200) {
          res.resume();
          return reject(new Error(`stream HTTP ${res.statusCode}`));
        }
        let buf = "";
        res.on("data", (chunk) => {
          buf += chunk.toString("utf8");
          let idx;
          while ((idx = buf.indexOf("\n")) >= 0) {
            const line = buf.slice(0, idx);
            buf = buf.slice(idx + 1);
            if (!line.startsWith("data: ")) continue;
            let ev;
            try { ev = JSON.parse(line.slice(6)); } catch { continue; }
            if (ev.id) cursor = ev.id;
            onEvent(ev);
            if (ev.type === "session.status_idle" || ev.type === "session.error") {
              req.destroy();
              return resolve(cursor);
            }
          }
        });
        res.on("end", () => resolve(cursor));
        res.on("error", () => resolve(cursor)); // destroyed after turn end
      });
      req.setTimeout(timeoutMs, () => { req.destroy(); reject(new Error("stream timeout")); });
      req.on("error", (e) => reject(e));
      req.end();
    });
  }
}
