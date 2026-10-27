/**
 * @file internalRemediationModule.ts
 * NestJS module for the internal remediation webhook bounded context: the
 * `InternalRemediationController` REST receiver, the `activeRemediationActions`
 * GraphQL query, and the in-memory `ActiveRemediationActionsRegistry` they share.
 */

import { Module } from '@nestjs/common';
import { InternalRemediationController } from './internalRemediationController';
import { InternalServiceAuthGuard } from './guards/internalServiceAuthGuard';
import { ActiveRemediationActionsRegistry } from './registry/activeRemediationActionsRegistry';
import { ActiveRemediationActionsResolver } from './graphql/activeRemediationActionsResolver';

@Module({
  controllers: [InternalRemediationController],
  providers: [ActiveRemediationActionsRegistry, InternalServiceAuthGuard, ActiveRemediationActionsResolver],
  exports: [ActiveRemediationActionsRegistry],
})
export class InternalRemediationModule {}
