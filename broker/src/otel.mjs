// OpenTelemetry for the broker: emits a span per MCP request and per tool call,
// exported to the same collector as the brain and hand so one trace shows
// brain -> broker -> hand. No-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set (so
// local/dev runs stay clean).
//
// Content is NOT recorded — only tool name, hand target and the policy decision,
// matching the brain/hand privacy posture.
//
// gRPC (:4317) everywhere: serve, the brain, the hand and Claude Code all export
// that way. An HTTP exporter posted to that port fails SILENTLY — no spans, no
// error — which is how the hand's tracing looked broken until the port was
// noticed. OTEL_EXPORTER_OTLP_PROTOCOL=grpc is set in the deployment.

import { trace, context, propagation, SpanStatusCode } from "@opentelemetry/api";
import { NodeSDK } from "@opentelemetry/sdk-node";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-grpc";

let started = false;

export function initOtel() {
  if (started || !process.env.OTEL_EXPORTER_OTLP_ENDPOINT) return;
  try {
    const sdk = new NodeSDK({
      serviceName: process.env.OTEL_SERVICE_NAME || "agentplane-broker",
      traceExporter: new OTLPTraceExporter(),
    });
    sdk.start();
    started = true;
    process.on("SIGTERM", () => sdk.shutdown().catch(() => {}));
    console.log(`broker: OTEL tracing on → ${process.env.OTEL_EXPORTER_OTLP_ENDPOINT}`);
  } catch (e) {
    console.error("broker: OTEL init failed:", e.message);
  }
}

const tracer = () => trace.getTracer("agentplane-broker");

/** The trace context carried by an incoming request's headers (from the brain),
 *  so broker spans nest under the caller's trace. Falls back to the active one. */
export function contextFromHeaders(headers) {
  try {
    return propagation.extract(context.active(), headers ?? {});
  } catch {
    return context.active();
  }
}

/** Run fn inside a span parented to parentCtx; fn receives the child context so
 *  the outgoing hand call can propagate it. Ends the span and records errors. */
export async function withSpan(name, parentCtx, attrs, fn) {
  let span;
  try {
    span = tracer().startSpan(name, { attributes: attrs }, parentCtx);
  } catch {
    return fn(parentCtx);
  }
  const childCtx = trace.setSpan(parentCtx, span);
  try {
    return await fn(childCtx);
  } catch (e) {
    try { span.setStatus({ code: SpanStatusCode.ERROR, message: String(e?.message).slice(0, 200) }); } catch {}
    throw e;
  } finally {
    try { span.end(); } catch {}
  }
}

/** W3C traceparent (and friends) for a context, to inject into the hand request
 *  so the hand nests its tool spans under ours — completing brain→broker→hand. */
export function headersForContext(ctx) {
  const carrier = {};
  try { propagation.inject(ctx, carrier); } catch {}
  return carrier;
}
