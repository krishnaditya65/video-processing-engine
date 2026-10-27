/**
 * @file OpenTelemetry SDK wiring for the analytics-telemetry-platform
 * service.
 *
 * Initializes a Node OpenTelemetry SDK instance so that this service's own
 * operations (Kafka consume, ClickHouse insert/query) are traced, and
 * exposes a small helper for extracting the active trace id so it can be
 * correlated with the `traceId` field client SDKs stamp onto playback
 * beacons (see `chunkProvenanceLookup.ts` for the linkage model back to
 * the originating encoding worker/container).
 *
 * This module intentionally does not start any network exporter by
 * default (no live collector is assumed to exist in this environment); it
 * wires a `ConsoleSpanExporter` so the structure is real and correct
 * against the actual `@opentelemetry/sdk-node` API, while remaining safe
 * to import without a running OTel Collector. Swap
 * `ConsoleSpanExporter` for `OTLPTraceExporter` (from
 * `@opentelemetry/exporter-trace-otlp-http`) in production.
 */
import { NodeSDK } from '@opentelemetry/sdk-node';
import { ConsoleSpanExporter } from '@opentelemetry/sdk-trace-base';
import { Resource } from '@opentelemetry/resources';
import { SemanticResourceAttributes } from '@opentelemetry/semantic-conventions';
import { context, trace, type Span } from '@opentelemetry/api';

/**
 * Module-level handle to the initialized `NodeSDK` instance, retained so
 * `shutdownAnalyticsTelemetryOpenTelemetrySdk` can gracefully flush spans
 * on process shutdown.
 */
let activeNodeSdk: NodeSDK | null = null;

/**
 * Initializes and starts the OpenTelemetry Node SDK for this service,
 * registering a `ConsoleSpanExporter` (development-safe default; swap for
 * an OTLP exporter pointed at a real Collector in production) and tagging
 * emitted spans with the service's resource attributes.
 *
 * Idempotent: calling this more than once is a no-op after the first call.
 *
 * @param serviceName - Logical service name to attach as the
 * `service.name` resource attribute on all emitted spans.
 */
export function initializeAnalyticsTelemetryOpenTelemetrySdk(
  serviceName: string = 'analytics-telemetry-platform',
): void {
  if (activeNodeSdk !== null) {
    return;
  }

  const sdk = new NodeSDK({
    resource: new Resource({
      [SemanticResourceAttributes.SERVICE_NAME]: serviceName,
      [SemanticResourceAttributes.SERVICE_VERSION]: '1.0.0',
    }),
    traceExporter: new ConsoleSpanExporter(),
  });

  sdk.start();
  activeNodeSdk = sdk;
}

/**
 * Gracefully shuts down the OpenTelemetry SDK, flushing any buffered
 * spans. Should be called on process `SIGTERM`/`SIGINT` handlers.
 */
export async function shutdownAnalyticsTelemetryOpenTelemetrySdk(): Promise<void> {
  if (activeNodeSdk === null) {
    return;
  }
  await activeNodeSdk.shutdown();
  activeNodeSdk = null;
}

/**
 * Extracts the W3C trace id of the currently active OpenTelemetry span, if
 * any. Used to stamp the `traceId` field of beacons/records this service
 * itself produces (e.g. re-publishing enriched beacons), and mirrors the
 * mechanism by which encoding workers stamp `traceId` onto
 * `ChunkProvenanceRecord`s and client SDKs echo it onto playback beacons.
 *
 * @returns The 32-hex-character trace id of the active span, or `null` if
 * no span is currently active on the context.
 */
export function extractActiveOpenTelemetryTraceId(): string | null {
  const activeSpan: Span | undefined = trace.getSpan(context.active());
  if (activeSpan === undefined) {
    return null;
  }
  const spanContext = activeSpan.spanContext();
  return spanContext.traceId ?? null;
}
