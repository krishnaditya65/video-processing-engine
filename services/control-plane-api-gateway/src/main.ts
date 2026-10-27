/**
 * @file main.ts
 * Process entry point: bootstraps the NestJS application, applies a global
 * `ValidationPipe` (so `class-validator` decorators on GraphQL input types are
 * enforced), and starts the HTTP/GraphQL listener on the configured port.
 */

import { Logger, ValidationPipe } from '@nestjs/common';
import { NestFactory } from '@nestjs/core';
import { AppModule } from './app.module';
import { ApplicationConfigurationService } from './config/applicationConfigurationService';

/**
 * Bootstraps and starts the Control Plane & API Gateway service.
 */
async function bootstrapControlPlaneApiGatewayService(): Promise<void> {
  const logger = new Logger('Bootstrap');
  const app = await NestFactory.create(AppModule);

  // Strips unknown properties and rejects requests with validation failures on any
  // GraphQL input type decorated with class-validator constraints.
  app.useGlobalPipes(new ValidationPipe({ whitelist: true, transform: true }));

  const applicationConfigurationService = app.get(ApplicationConfigurationService);
  const { httpServerListenPort } = applicationConfigurationService.getRuntimeConfiguration();

  await app.listen(httpServerListenPort);
  logger.log(`Control Plane & API Gateway listening on port ${httpServerListenPort} (GraphQL endpoint: /graphql).`);
}

bootstrapControlPlaneApiGatewayService().catch((bootstrapError) => {
  // eslint-disable-next-line no-console
  console.error('Fatal error during application bootstrap:', bootstrapError);
  process.exitCode = 1;
});
