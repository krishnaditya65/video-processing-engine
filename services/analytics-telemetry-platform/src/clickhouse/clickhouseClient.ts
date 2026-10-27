/**
 * @file ClickHouse client factory for the analytics-telemetry-platform
 * service, built on the real `@clickhouse/client` package.
 *
 * This module owns constructing and configuring the shared
 * `ClickHouseClient` instance used by `PlaybackBeaconRepository`. It is
 * structurally correct against the real `@clickhouse/client` API surface
 * (`createClient`, `.insert`, `.query`, `.exec`, `.close`) but is not
 * exercised against a live ClickHouse cluster in this repository's test
 * suite -- see README.md "Testing Strategy" for what is/isn't covered by
 * `npm test`.
 */
import { createClient, type ClickHouseClient } from '@clickhouse/client';

/** Configuration required to construct a ClickHouse client connection. */
export interface ClickHouseConnectionConfig {
  /** ClickHouse HTTP interface URL, e.g. "http://localhost:8123". */
  url: string;
  /** ClickHouse username. */
  username: string;
  /** ClickHouse password. */
  password: string;
  /** Target database name for all queries issued through this client. */
  database: string;
  /**
   * Maximum number of sockets held open in the underlying HTTP keep-alive
   * pool -- tuned up from the client default for a high-throughput
   * ingestion workload issuing frequent micro-batch inserts.
   */
  maxOpenConnections: number;
  /** Request timeout, in milliseconds, applied to insert/query calls. */
  requestTimeoutMillis: number;
}

/**
 * Builds a `ClickHouseConnectionConfig` from process environment
 * variables, applying sane production-lean defaults for anything unset.
 *
 * @returns A fully populated `ClickHouseConnectionConfig`.
 */
export function buildClickHouseConnectionConfigFromEnvironment(): ClickHouseConnectionConfig {
  return {
    url: process.env['CLICKHOUSE_URL'] ?? 'http://localhost:8123',
    username: process.env['CLICKHOUSE_USERNAME'] ?? 'default',
    password: process.env['CLICKHOUSE_PASSWORD'] ?? '',
    database: process.env['CLICKHOUSE_DATABASE'] ?? 'telemetry_lake',
    maxOpenConnections: Number(process.env['CLICKHOUSE_MAX_OPEN_CONNECTIONS'] ?? 32),
    requestTimeoutMillis: Number(process.env['CLICKHOUSE_REQUEST_TIMEOUT_MILLIS'] ?? 30_000),
  };
}

/**
 * Constructs a configured `ClickHouseClient` from `@clickhouse/client`.
 *
 * @param config - Connection configuration, typically produced by
 * `buildClickHouseConnectionConfigFromEnvironment`.
 * @returns A ready-to-use `ClickHouseClient` instance.
 */
export function createConfiguredClickHouseClient(config: ClickHouseConnectionConfig): ClickHouseClient {
  return createClient({
    url: config.url,
    username: config.username,
    password: config.password,
    database: config.database,
    max_open_connections: config.maxOpenConnections,
    request_timeout: config.requestTimeoutMillis,
    clickhouse_settings: {
      // Async insert settings tuned for high-frequency micro-batch writes;
      // see README "Ingestion Throughput Targets" for the latency/efficiency
      // tradeoff this configuration embodies.
      async_insert: 1,
      wait_for_async_insert: 0,
    },
  });
}
