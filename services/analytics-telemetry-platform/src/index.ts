/**
 * @file Service entry point for the analytics-telemetry-platform
 * microservice.
 *
 * Wires together OpenTelemetry initialization, the ClickHouse client and
 * repository, the chunk-provenance lookup, the micro-batch accumulator,
 * and the Kafka/Redpanda consumer, then starts consuming. This file
 * performs real I/O on startup (connects to Redpanda and ClickHouse) and
 * is therefore not invoked by the Jest test suite -- see README.md
 * "Testing Strategy". It exists to demonstrate the fully-wired,
 * structurally correct composition root for the service.
 */
import Redis from 'ioredis';
import { initializeAnalyticsTelemetryOpenTelemetrySdk, shutdownAnalyticsTelemetryOpenTelemetrySdk } from './tracing/otelSetup';
import { InMemoryChunkProvenanceLookup, type ChunkProvenanceLookup } from './tracing/chunkProvenanceLookup';
import { RedisBackedChunkProvenanceLookup } from './tracing/redisBackedChunkProvenanceLookup';
import { buildClickHouseConnectionConfigFromEnvironment, createConfiguredClickHouseClient } from './clickhouse/clickhouseClient';
import { PlaybackBeaconRepository } from './clickhouse/playbackBeaconRepository';
import { BeaconMicroBatchAccumulator } from './ingestion/pipeline/beaconBatchInsertStage';
import { PlaybackBeaconKafkaConsumer } from './ingestion/kafkaConsumer';
import { AnomalyEventEmitter } from './anomaly/anomalyEventEmitter';
import { ManifestFilterRuleEmitter } from './anomaly/manifestFilterRuleEmitter';
import { EncodingExperimentAutoRemediationService } from './abtest/encodingExperimentAutoRemediationService';
import { ConcurrentViewerAggregator } from './metrics/concurrentViewerAggregator';

/**
 * Boots the analytics-telemetry-platform service: initializes tracing,
 * constructs the ClickHouse repository and micro-batch accumulator, and
 * starts the Kafka/Redpanda consumer loop.
 */
async function main(): Promise<void> {
  initializeAnalyticsTelemetryOpenTelemetrySdk();

  const clickHouseClient = createConfiguredClickHouseClient(buildClickHouseConnectionConfigFromEnvironment());
  const playbackBeaconRepository = new PlaybackBeaconRepository(clickHouseClient);

  const microBatchAccumulator = new BeaconMicroBatchAccumulator(playbackBeaconRepository, {
    // See README "Ingestion Throughput Targets" for the rationale behind
    // these defaults.
    maxBatchSizeRows: Number(process.env['BEACON_MICROBATCH_MAX_ROWS'] ?? 50_000),
    maxBatchLingerMillis: Number(process.env['BEACON_MICROBATCH_MAX_LINGER_MILLIS'] ?? 500),
  });

  // Chunk provenance lookup: back it with Redis when REDIS_URL is
  // configured (the production, multi-replica-safe path -- see
  // `RedisBackedChunkProvenanceLookup`); otherwise fall back to the
  // in-memory reference implementation for single-instance/local dev.
  const redisUrl = process.env['REDIS_URL'];
  const chunkProvenanceLookup: ChunkProvenanceLookup = redisUrl
    ? new RedisBackedChunkProvenanceLookup(new Redis(redisUrl))
    : new InMemoryChunkProvenanceLookup();

  // Real-time CCU aggregation from beacon heartbeats (see
  // `ConcurrentViewerAggregator`); the periodic-eviction timer bounds
  // memory during idle periods and is stopped again on shutdown below.
  const concurrentViewerAggregator = new ConcurrentViewerAggregator();
  const stopConcurrentViewerEviction = concurrentViewerAggregator.startPeriodicEviction();

  // Live A/B rollback automation and anomaly auto-remediation: both close
  // the loop from "decision/detection" to "action" via an outbound webhook
  // to a sibling service. See README "Outbound Webhook Contracts" for the
  // full documented request/response contract each webhook implements.
  const encodingExperimentAutoRemediationService = new EncodingExperimentAutoRemediationService({
    controlPlaneBaseUrl: process.env['CONTROL_PLANE_BASE_URL'] ?? 'http://localhost:4000',
  });
  const manifestFilterRuleEmitter = new ManifestFilterRuleEmitter({
    edgePackagingBaseUrl: process.env['EDGE_PACKAGING_BASE_URL'] ?? 'http://localhost:4100',
  });
  const firmwareCohortAnomalyEventEmitter = new AnomalyEventEmitter();
  manifestFilterRuleEmitter.subscribeToAnomalyEvents(firmwareCohortAnomalyEventEmitter);

  const consumer = new PlaybackBeaconKafkaConsumer(
    {
      brokers: (process.env['REDPANDA_BROKERS'] ?? 'localhost:9092').split(','),
      consumerGroupId: process.env['REDPANDA_CONSUMER_GROUP_ID'] ?? 'analytics-telemetry-platform',
      topic: process.env['REDPANDA_BEACON_TOPIC'] ?? 'qoe-playback-beacons',
      clientId: 'analytics-telemetry-platform',
      maxBatchSizeRows: 50_000,
      maxBatchLingerMillis: 500,
    },
    chunkProvenanceLookup,
    microBatchAccumulator,
  );

  const shutdown = async (): Promise<void> => {
    await consumer.stopConsuming();
    stopConcurrentViewerEviction();
    await shutdownAnalyticsTelemetryOpenTelemetrySdk();
    process.exit(0);
  };
  process.on('SIGTERM', () => void shutdown());
  process.on('SIGINT', () => void shutdown());

  await consumer.startConsuming();
}

if (require.main === module) {
  main().catch((error) => {
    // eslint-disable-next-line no-console
    console.error('Fatal error starting analytics-telemetry-platform', error);
    process.exit(1);
  });
}

export {
  initializeAnalyticsTelemetryOpenTelemetrySdk,
  shutdownAnalyticsTelemetryOpenTelemetrySdk,
  InMemoryChunkProvenanceLookup,
  RedisBackedChunkProvenanceLookup,
  buildClickHouseConnectionConfigFromEnvironment,
  createConfiguredClickHouseClient,
  PlaybackBeaconRepository,
  BeaconMicroBatchAccumulator,
  PlaybackBeaconKafkaConsumer,
  AnomalyEventEmitter,
  ManifestFilterRuleEmitter,
  EncodingExperimentAutoRemediationService,
  ConcurrentViewerAggregator,
};
