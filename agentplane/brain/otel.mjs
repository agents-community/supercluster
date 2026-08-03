// Trace continuity from serve into the brain.
//
// serve is the trace root and propagates W3C traceparent, which atenet's Envoy
// joins and forwards. It stopped there: the brain had no instrumentation, so
// serve's trace ended at the HTTP hop and the turn's real work appeared in a
// SEPARATE trace started by Claude Code's own telemetry. Answering "why was
// this request slow" meant finding two traces and lining them up by timestamp.
//
// What this does NOT do: reparent Claude Code's internal spans. The harness is
// one long-lived stream consuming a queue across many turns, so no per-request
// context wraps it, and the SDK exposes no way to hand it a parent per turn.
// Instead the brain emits its own `brain.turn` span as a genuine child of
// serve's trace, and stamps the harness session id on it — so one trace shows
// request -> routing -> turn duration, and the id ties it to the claude-code
// trace holding the model and tool spans.
//
// This registers its OWN tracer provider rather than reusing Claude Code's.
// That looked wrong at first — two SDKs in one process — but Claude Code is
// installed globally (`npm install -g`) while this app lives in /app, so the
// two resolve DIFFERENT copies of @opentelemetry/api. Each copy keeps its own
// global registry, so the provider Claude Code registers is invisible here:
// piggybacking produced silent no-op spans and nothing reached Jaeger. They do
// not contend, because neither can see the other's registration.
//
// Inert when OTEL_EXPORTER_OTLP_ENDPOINT is unset, so local runs stay quiet.

import { trace, context, propagation, SpanStatusCode } from "@opentelemetry/api";
import { NodeSDK } from "@opentelemetry/sdk-node";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-grpc";

let started = false;

export function initOtel() {
  if (started || !process.env.OTEL_EXPORTER_OTLP_ENDPOINT) return;
  try {
    // gRPC, not HTTP: the brain's OTEL_EXPORTER_OTLP_ENDPOINT is the :4317
    // gRPC port (set for Claude Code, with OTEL_EXPORTER_OTLP_PROTOCOL=grpc).
    // An HTTP exporter posts to that port and fails silently — no spans, no
    // error. Every component now exports gRPC on :4317.
    //
    // serviceName is set HERE rather than via OTEL_SERVICE_NAME, because that
    // env var would also rename Claude Code's spans and collapse the two
    // services into one in Jaeger.
    const sdk = new NodeSDK({
      serviceName: "agentplane-brain",
      traceExporter: new OTLPTraceExporter(),
    });
    sdk.start();
    started = true;
    process.on("SIGTERM", () => sdk.shutdown().catch(() => {}));
    console.log(`brain: OTEL tracing on → ${process.env.OTEL_EXPORTER_OTLP_ENDPOINT}`);
  } catch (e) {
    console.error("brain: OTEL init failed:", e.message);
  }
}

const tracer = () => trace.getTracer("agentplane-brain");

/**
 * Extract the incoming W3C trace context from a Node request's headers.
 * Returns a Context suitable as a span parent; falls back to the active one.
 */
export function contextFromRequest(req) {
  try {
    return propagation.extract(context.active(), req.headers ?? {});
  } catch {
    return context.active();
  }
}

/**
 * Start the span covering one turn, parented to the caller's trace.
 *
 * The turn completes asynchronously, long after the HTTP response — so this
 * returns a handle the runtime ends when the turn actually finishes, rather
 * than wrapping a callback that would close at the wrong moment.
 */
export function startTurnSpan(parentCtx, attrs = {}) {
  let span;
  try {
    span = tracer().startSpan("brain.turn", { attributes: attrs }, parentCtx);
  } catch {
    return noopTurn;
  }
  return {
    // The harness session id is the join key to the claude-code trace.
    setHarnessSession(id) {
      try { if (id) span.setAttribute("agentplane.harness_session", id); } catch {}
    },
    setAttribute(k, v) {
      try { span.setAttribute(k, v); } catch {}
    },
    fail(message) {
      try {
        span.setStatus({ code: SpanStatusCode.ERROR, message: String(message).slice(0, 200) });
      } catch {}
    },
    end() {
      try { span.end(); } catch {}
    },
  };
}

const noopTurn = {
  setHarnessSession() {}, setAttribute() {}, fail() {}, end() {},
};
