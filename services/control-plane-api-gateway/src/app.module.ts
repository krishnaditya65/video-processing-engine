/**
 * @file app.module.ts
 * Root NestJS module. Wires up the code-first GraphQL driver (Apollo), the global
 * Redis module, JWT signing infrastructure, application configuration, and every
 * domain feature module (asset registry, playback session, encoding orchestration).
 */

import { Module } from '@nestjs/common';
import { ApolloDriver, ApolloDriverConfig } from '@nestjs/apollo';
import { GraphQLModule } from '@nestjs/graphql';
import { JwtModule } from '@nestjs/jwt';
import { join } from 'path';
import { ApplicationConfigurationModule } from './config/applicationConfigurationModule';
import { ApplicationConfigurationService } from './config/applicationConfigurationService';
import { RedisModule } from './redis/redisModule';
import { AssetRegistryModule } from './asset-registry/assetRegistryModule';
import { PlaybackSessionModule } from './playback-session/playbackSessionModule';
import { EncodingOrchestrationModule } from './encoding-orchestration/encodingOrchestrationModule';
import { InternalRemediationModule } from './internal-remediation/internalRemediationModule';

@Module({
  imports: [
    ApplicationConfigurationModule,
    // Code-first GraphQL: the schema is generated from the TypeScript decorators on
    // resolvers/DTOs (see `graphql/schema.gql` output path below) rather than
    // hand-written SDL, keeping the schema and TypeScript types from drifting apart.
    GraphQLModule.forRoot<ApolloDriverConfig>({
      driver: ApolloDriver,
      autoSchemaFile: join(process.cwd(), 'src/graphql/schema.gql'),
      sortSchema: true,
      // Attaches the raw Express request to the GraphQL context so guards/decorators
      // (JsonWebTokenAuthenticationGuard, RedisBackedRateLimitingGuard,
      // CurrentAuthenticatedUser) can read headers/IP/attached auth state.
      context: ({ req }: { req: unknown }) => ({ req }),
      // Surfaces stack traces in GraphQL error responses only outside production, so
      // local development/demoing is easier without leaking internals in a real deploy.
      includeStacktraceInErrorResponses: process.env.NODE_ENV !== 'production',
    }),
    RedisModule,
    JwtModule.registerAsync({
      // Registered globally so JwtService is injectable into
      // JsonWebTokenAuthenticationGuard from any feature module without each one
      // re-importing JwtModule individually.
      global: true,
      inject: [ApplicationConfigurationService],
      useFactory: (applicationConfigurationService: ApplicationConfigurationService) => ({
        secret: applicationConfigurationService.getRuntimeConfiguration().jsonWebTokenSigningSecret,
        signOptions: { expiresIn: '1h' },
      }),
    }),
    AssetRegistryModule,
    PlaybackSessionModule,
    EncodingOrchestrationModule,
    InternalRemediationModule,
  ],
})
export class AppModule {}
