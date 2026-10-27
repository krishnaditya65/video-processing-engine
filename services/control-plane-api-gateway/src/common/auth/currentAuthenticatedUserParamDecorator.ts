/**
 * @file currentAuthenticatedUserParamDecorator.ts
 * A custom Nest parameter decorator that lets resolvers pull the verified
 * {@link AuthenticatedUserPayload} (attached by {@link JsonWebTokenAuthenticationGuard})
 * directly as a method argument, e.g.:
 * ```ts
 * @Mutation(() => PlaybackSessionGraphQlType)
 * @UseGuards(JsonWebTokenAuthenticationGuard)
 * createPlaybackSession(@CurrentAuthenticatedUser() user: AuthenticatedUserPayload) { ... }
 * ```
 */

import { ExecutionContext, createParamDecorator } from '@nestjs/common';
import { GqlExecutionContext } from '@nestjs/graphql';
import { AuthenticatedUserPayload } from './authenticatedUserPayload.interface';

/**
 * Extracts the previously-verified authenticated user payload from the GraphQL
 * request context. Must be used downstream of {@link JsonWebTokenAuthenticationGuard}
 * (or another guard populating `request.authenticatedUser`) — if no guard has run,
 * this resolves to `undefined`, so resolvers requiring auth should always also apply
 * the guard rather than relying on this decorator alone for enforcement.
 * @returns The {@link AuthenticatedUserPayload} attached to the current request, or `undefined` if unauthenticated.
 */
export const CurrentAuthenticatedUser = createParamDecorator(
  (_decoratorConfigurationData: unknown, executionContext: ExecutionContext): AuthenticatedUserPayload | undefined => {
    const gqlExecutionContext = GqlExecutionContext.create(executionContext);
    const request = gqlExecutionContext.getContext<{ req?: { authenticatedUser?: AuthenticatedUserPayload } }>().req;
    return request?.authenticatedUser;
  },
);
