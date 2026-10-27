/**
 * @file registerRawVideoAssetInputType.ts
 * Code-first GraphQL input type + `class-validator` decorators for the
 * `registerRawVideoAsset` mutation. Validation is enforced by Nest's global
 * `ValidationPipe` (configured in `main.ts`).
 */

import { Field, InputType } from '@nestjs/graphql';
import { IsNotEmpty, IsString, MaxLength, MinLength } from 'class-validator';

/** Input payload for registering a newly-uploaded raw video asset. */
@InputType('RegisterRawVideoAssetInput')
export class RegisterRawVideoAssetInputType {
  @Field(() => String, { description: 'Human-readable title for editorial/CMS display.' })
  @IsString()
  @IsNotEmpty()
  @MaxLength(300)
  displayTitle!: string;

  @Field(() => String, { description: 'Storage URI (e.g. s3://bucket/key.mov) of the raw mezzanine upload.' })
  @IsString()
  @MinLength(5)
  @MaxLength(2048)
  sourceMezzanineStorageUri!: string;
}
