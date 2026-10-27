/**
 * @file Jest configuration for the analytics-telemetry-platform service.
 *
 * Only the pure/business-logic layers (QoE metrics, anomaly statistics,
 * A/B significance tests, beacon validation pipeline) are exercised by
 * the test suite, since those layers have no external I/O dependency
 * (no live Kafka/Redpanda broker or ClickHouse cluster required). See
 * README.md, section "Testing Strategy", for the full rationale.
 */
module.exports = {
  preset: 'ts-jest',
  testEnvironment: 'node',
  rootDir: '.',
  roots: ['<rootDir>/tests'],
  testMatch: ['**/*.test.ts'],
  collectCoverageFrom: [
    'src/metrics/**/*.ts',
    'src/anomaly/**/*.ts',
    'src/abtest/**/*.ts',
    'src/ingestion/pipeline/**/*.ts',
    'src/analytics/**/*.ts',
    'src/webhooks/**/*.ts',
    'src/tracing/redisBackedChunkProvenanceLookup.ts',
  ],
  moduleNameMapper: {
    '^@src/(.*)$': '<rootDir>/src/$1',
  },
  verbose: true,
};
