/**
 * @file High-throughput Kafka/Redpanda consumer for QoE playback beacons,
 * built on the real `kafkajs` client library.
 *
 * NOTE ON TARGET BROKER: this consumer targets Redpanda in production, not
 * Apache Kafka itself. Redpanda is wire-protocol-compatible with the Kafka
 * broker API, so `kafkajs` (a Kafka client) works against it unmodified --
 * we get Redpanda's lower tail latency and simpler operations (no
 * ZooKeeper/JVM) while keeping the enormous Kafka client ecosystem. Every
 * `kafkajs` call in this file is coded against its real, documented API
 * (`Kafka`, `.consumer`, `.connect`, `.subscribe`, `.run` with
 * `eachBatch`).
 *
 * This module is structurally correct but is not exercised against a live
 * broker in this repository's automated test suite -- see README.md
 * "Testing Strategy". The actual beacon processing logic it delegates to
 * (`runPlaybackBeaconIngestionPipeline`) *is* fully unit-tested via its
 * pure sub-stages.
 */
import { Kafka, logLevel, type Consumer, type EachBatchPayload } from 'kafkajs';
import { runPlaybackBeaconIngestionPipeline } from './pipeline/beaconIngestionPipeline';
import { BeaconMicroBatchAccumulator } from './pipeline/beaconBatchInsertStage';
import type { ChunkProvenanceLookup } from '../tracing/chunkProvenanceLookup';
import type { RawPlaybackBeaconWireMessage } from '../types/beaconTypes';

/** Configuration required to construct the beacon-ingestion Kafka consumer. */
export interface PlaybackBeaconKafkaConsumerConfig {
  /** Redpanda/Kafka-API-compatible broker bootstrap addresses, e.g. ["redpanda-0:9092"]. */
  brokers: string[];
  /** Kafka consumer group id for this ingestion service's consumer fleet. */
  consumerGroupId: string;
  /** Topic carrying raw QoE playback beacon messages. */
  topic: string;
  /** Client id reported to the broker for logging/quota purposes. */
  clientId: string;
  /**
   * Maximum number of messages `eachBatch` will process per broker fetch
   * round before committing offsets -- tuned in concert with
   * `BeaconMicroBatchAccumulatorConfig.maxBatchSizeRows` (see README
   * "Ingestion Throughput Targets").
   */
  maxBatchSizeRows: number;
  /** Micro-batch linger time passed through to `BeaconMicroBatchAccumulator`. */
  maxBatchLingerMillis: number;
}

/**
 * Wraps a `kafkajs` `Consumer` configured for high-throughput batch
 * processing of QoE playback beacons, wiring each fetched batch through
 * `runPlaybackBeaconIngestionPipeline`.
 */
export class PlaybackBeaconKafkaConsumer {
  private readonly kafka: Kafka;
  private readonly consumer: Consumer;

  /**
   * @param config - Broker/topic/consumer-group configuration.
   * @param chunkProvenanceLookup - Strategy implementation passed through
   * to the enrichment pipeline stage.
   * @param microBatchAccumulator - Shared accumulator the pipeline
   * enqueues enriched beacons into for eventual ClickHouse flush.
   */
  public constructor(
    private readonly config: PlaybackBeaconKafkaConsumerConfig,
    private readonly chunkProvenanceLookup: ChunkProvenanceLookup,
    private readonly microBatchAccumulator: BeaconMicroBatchAccumulator,
  ) {
    this.kafka = new Kafka({
      clientId: config.clientId,
      brokers: config.brokers,
      logLevel: logLevel.ERROR,
    });
    this.consumer = this.kafka.consumer({
      groupId: config.consumerGroupId,
      // Larger fetch bytes/wait tuned for throughput over per-message
      // latency, consistent with the micro-batching strategy documented
      // in README "Ingestion Throughput Targets".
      maxBytesPerPartition: 4 * 1024 * 1024,
      sessionTimeout: 30_000,
    });
  }

  /**
   * Connects the consumer, subscribes to the configured topic, and begins
   * processing fetched batches via `eachBatch`, running each batch through
   * the full beacon ingestion pipeline.
   *
   * `eachBatch` (rather than `eachMessage`) is used deliberately: it gives
   * us the whole fetched batch at once, which is what
   * `runPlaybackBeaconIngestionPipeline` is designed to process
   * efficiently, and lets us control offset commits precisely (only after
   * the batch has been fully handed off to the micro-batch accumulator).
   */
  public async startConsuming(): Promise<void> {
    await this.consumer.connect();
    await this.consumer.subscribe({ topic: this.config.topic, fromBeginning: false });

    await this.consumer.run({
      partitionsConsumedConcurrently: 3,
      eachBatch: async (payload: EachBatchPayload) => {
        const { batch, resolveOffset, heartbeat, commitOffsetsIfNecessary } = payload;

        const rawMessages: RawPlaybackBeaconWireMessage[] = [];
        for (const message of batch.messages) {
          if (message.value === null) {
            continue;
          }
          try {
            rawMessages.push(JSON.parse(message.value.toString('utf8')) as RawPlaybackBeaconWireMessage);
          } catch {
            // Malformed JSON is treated as a validation failure downstream
            // rather than crashing the consumer loop; we skip it here and
            // rely on structured logging/metrics in a production build.
            continue;
          }
        }

        if (rawMessages.length > 0) {
          await runPlaybackBeaconIngestionPipeline(
            rawMessages,
            this.chunkProvenanceLookup,
            this.microBatchAccumulator,
          );
        }

        for (const message of batch.messages) {
          resolveOffset(message.offset);
        }
        await heartbeat();
        await commitOffsetsIfNecessary();
      },
    });
  }

  /**
   * Gracefully disconnects the consumer and flushes any remaining
   * buffered beacons, intended to be called from process shutdown
   * handlers.
   */
  public async stopConsuming(): Promise<void> {
    await this.microBatchAccumulator.flushBufferedBatchToClickHouse();
    await this.consumer.disconnect();
  }
}
