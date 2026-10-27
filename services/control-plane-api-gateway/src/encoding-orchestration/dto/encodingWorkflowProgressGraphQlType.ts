/**
 * @file encodingWorkflowProgressGraphQlType.ts
 * GraphQL object type exposing live `distributedEncodingWorkflow` progress, sourced
 * via {@link EncodingOrchestratorClientService} (circuit-breaker protected).
 */

import { Field, Int, ObjectType } from '@nestjs/graphql';

@ObjectType('EncodingWorkflowProgress', { description: 'Live progress snapshot of a distributedEncodingWorkflow run.' })
export class EncodingWorkflowProgressGraphQlType {
  @Field(() => String)
  currentStage!: string;

  @Field(() => Int)
  totalSceneCount!: number;

  @Field(() => Int)
  completedSceneCount!: number;
}
