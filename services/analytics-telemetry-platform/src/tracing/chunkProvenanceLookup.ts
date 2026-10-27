/**
 * @file Micro-session distributed tracing: traceId -> chunk provenance
 * linkage abstraction.
 *
 * Linkage model (simplified, documented):
 * -----------------------------------------------------------------------
 * Every video chunk (a short, independently encodable segment of the
 * output ABR ladder, typically 2-6 seconds) is produced by exactly one
 * encoding worker/container instance in the transcoding fleet. When a
 * worker finishes encoding a chunk, it emits a `ChunkProvenanceRecord`
 * (worker id, container id, chunk id, encode timestamp) tagged with the
 * OpenTelemetry `traceId` of the encoding job's trace, and publishes it to
 * a short-lived provenance store (in production: a Redis hash / compacted
 * Kafka topic keyed by traceId, with a TTL matching the CDN edge cache
 * TTL -- typically minutes to a few hours).
 *
 * The player SDK propagates that same `traceId` back onto every QoE
 * beacon it emits while playing chunks from that encode (via the
 * `traceparent` header injected at chunk-request time, per the W3C Trace
 * Context spec, and echoed into the beacon payload's `traceId` field --
 * see `otelSetup.ts`). This lets the enrichment pipeline stage join a
 * beacon back to "which encoding worker produced the chunk this beacon is
 * reporting on" with a simple keyed lookup, without needing to thread
 * worker identity through the entire player/CDN path explicitly.
 *
 * `ChunkProvenanceLookup` is the Strategy-pattern-friendly abstraction over
 * that store: this file defines the interface plus an in-memory
 * implementation suitable for tests/local dev. A production implementation
 * would back it with Redis (`GET provenance:{traceId}`) or an equivalent
 * low-latency KV store.
 */

/** A single chunk-provenance record published by an encoding worker. */
export interface ChunkProvenanceRecord {
  /** OpenTelemetry trace id of the encoding job that produced the chunk. */
  traceId: string;
  /** Identifier of the encoding worker/container instance that produced the chunk. */
  encodingWorkerId: string;
  /** Identifier of the specific video chunk/segment produced. */
  chunkId: string;
  /** Epoch-milliseconds timestamp at which the chunk finished encoding. */
  encodedAtEpochMillis: number;
}

/**
 * Strategy interface for resolving a distributed trace id to the
 * chunk-provenance record describing which encoding worker produced the
 * associated video chunk. Abstracted so the enrichment pipeline stage can
 * be unit-tested against an in-memory fake and swapped to a Redis-backed
 * implementation in production without changing calling code.
 */
export interface ChunkProvenanceLookup {
  /**
   * Resolves a trace id to its chunk-provenance record.
   *
   * @param traceId - Distributed trace id propagated from an encoding job
   * onto a playback beacon.
   * @returns The matching `ChunkProvenanceRecord`, or `null` if no
   * provenance record exists for this trace id (e.g. it expired from the
   * lookup window, or the beacon was never tagged with a trace id).
   */
  resolveChunkProvenanceForTraceId(traceId: string): Promise<ChunkProvenanceRecord | null>;

  /**
   * Registers a chunk-provenance record, called by the encoding worker's
   * OpenTelemetry-instrumented publish path when a chunk finishes
   * encoding.
   *
   * @param record - The provenance record to store, keyed by its traceId.
   */
  registerChunkProvenanceRecord(record: ChunkProvenanceRecord): Promise<void>;
}

/**
 * In-memory implementation of `ChunkProvenanceLookup`, suitable for local
 * development, tests, and as a reference implementation. Not suitable for
 * multi-instance production deployment (state is not shared across
 * service replicas) -- production should back this interface with Redis
 * or an equivalent shared low-latency store keyed by traceId, with a TTL
 * eviction policy matching CDN edge cache TTL.
 */
export class InMemoryChunkProvenanceLookup implements ChunkProvenanceLookup {
  private readonly provenanceRecordsByTraceId = new Map<string, ChunkProvenanceRecord>();

  /** @inheritdoc */
  public async resolveChunkProvenanceForTraceId(traceId: string): Promise<ChunkProvenanceRecord | null> {
    return this.provenanceRecordsByTraceId.get(traceId) ?? null;
  }

  /** @inheritdoc */
  public async registerChunkProvenanceRecord(record: ChunkProvenanceRecord): Promise<void> {
    this.provenanceRecordsByTraceId.set(record.traceId, record);
  }
}
