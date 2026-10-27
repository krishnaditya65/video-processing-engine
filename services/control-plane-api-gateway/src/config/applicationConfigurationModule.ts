/**
 * @file applicationConfigurationModule.ts
 * Global NestJS module exposing {@link ApplicationConfigurationService} to every
 * other module in the application without each one needing to explicitly import it —
 * configuration access is a cross-cutting concern used by nearly every feature
 * module (Redis, JWT, Temporal, rate limiting, circuit breaker thresholds, etc.).
 */

import { Global, Module } from '@nestjs/common';
import { ApplicationConfigurationService } from './applicationConfigurationService';

@Global()
@Module({
  providers: [ApplicationConfigurationService],
  exports: [ApplicationConfigurationService],
})
export class ApplicationConfigurationModule {}
