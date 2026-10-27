/**
 * @file appModule.e2e-spec.ts
 * A minimal end-to-end smoke test verifying the full {@link AppModule} dependency
 * graph compiles and resolves via Nest's testing module builder — i.e. every module
 * import, provider, and injection token across the whole application (GraphQL,
 * Redis, JWT, asset registry, playback session, encoding orchestration) is wired
 * correctly. Deliberately does NOT call `app.listen()` or exercise real network I/O
 * against Redis/Temporal, so this test remains runnable without either being
 * available (see SERVICE_README.md §8-9 for what full live-integration testing would
 * additionally require).
 *
 * Run with `npm run test:e2e`.
 */

import { Test, TestingModule } from '@nestjs/testing';
import { AppModule } from '../src/app.module';

describe('AppModule (e2e wiring smoke test)', () => {
  let compiledTestingModule: TestingModule;

  afterAll(async () => {
    await compiledTestingModule?.close();
  });

  it('compiles the full application dependency graph without error', async () => {
    compiledTestingModule = await Test.createTestingModule({ imports: [AppModule] }).compile();
    expect(compiledTestingModule).toBeDefined();
  });
});
