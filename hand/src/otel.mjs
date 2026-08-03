// OpenTelemetry for the hand: emits a span per tool execution (bash, write,
// edit, grep, git via bash, federated calls …) so tool work shows up in the
// same collector/Jaeger as the brain's LLM spans. No-op unless
// OTEL_EXPORTER_OTLP_ENDPOINT is set (so local/dev runs stay clean).
//
// Content is NOT recorded (no command strings, no file contents) — only the
// tool name and success/failure — matching the brain's privacy posture.
//
// gRPC (:4317) everywhere: serve, the brain, and Claude Code all export that
// way, and the hand was the lone HTTP/:4318 exception. Mixing them means an
// exporter pointed at the wrong port POSTs into a gRPC listener and fails
// SILENTLY — no spans, no error — which is exactly how the brain's tracing
// looked broken until the port was noticed.

import { NodeSDK } from "@opentelemetry/sdk-node";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-grpc";

export function initOtel() {
  if (!process.env.OTEL_EXPORTER_OTLP_ENDPOINT) return;
  try {
    const sdk = new NodeSDK({ traceExporter: new OTLPTraceExporter() });
    sdk.start();
    process.on("SIGTERM", () => sdk.shutdown().catch(() => {}));
    console.log(`hand: OTEL tracing on → ${process.env.OTEL_EXPORTER_OTLP_ENDPOINT}`);
  } catch (e) {
    console.error("hand: OTEL init failed:", e.message);
  }
}
