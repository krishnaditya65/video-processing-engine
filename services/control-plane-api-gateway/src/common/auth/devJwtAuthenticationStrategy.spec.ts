/**
 * @file devJwtAuthenticationStrategy.spec.ts
 * Unit tests for {@link DevJwtAuthenticationStrategy}: valid token verification/claim
 * mapping, expired token rejection, and malformed token rejection.
 */

import { JwtService } from '@nestjs/jwt';
import { UnauthorizedException } from '@nestjs/common';
import { DevJwtAuthenticationStrategy } from './devJwtAuthenticationStrategy';

describe('DevJwtAuthenticationStrategy', () => {
  const testSigningSecret = 'unit-test-signing-secret';
  let jwtService: JwtService;
  let devJwtAuthenticationStrategy: DevJwtAuthenticationStrategy;

  beforeEach(() => {
    jwtService = new JwtService({ secret: testSigningSecret });
    devJwtAuthenticationStrategy = new DevJwtAuthenticationStrategy(jwtService);
  });

  it('reports its strategy identifier as "dev-jwt"', () => {
    expect(devJwtAuthenticationStrategy.strategyIdentifier).toBe('dev-jwt');
  });

  it('verifies a valid token and maps claims to an AuthenticatedUserPayload', async () => {
    const token = jwtService.sign(
      { sub: 'user-123', email: 'person@example.com', roles: ['admin'] },
      { expiresIn: '1h' },
    );
    const result = await devJwtAuthenticationStrategy.validateBearerTokenAndResolveAuthenticatedUser(token);
    expect(result.subjectUserIdentifier).toBe('user-123');
    expect(result.displayNameOrEmail).toBe('person@example.com');
    expect(result.grantedRoles).toEqual(['admin']);
    expect(result.tokenExpiresAtEpochSeconds).toBeGreaterThan(0);
  });

  it('defaults missing optional claims sensibly', async () => {
    const token = jwtService.sign({});
    const result = await devJwtAuthenticationStrategy.validateBearerTokenAndResolveAuthenticatedUser(token);
    expect(result.subjectUserIdentifier).toBe('unknown-subject');
    expect(result.displayNameOrEmail).toBeUndefined();
    expect(result.grantedRoles).toEqual([]);
  });

  it('rejects an expired token', async () => {
    const expiredToken = jwtService.sign({ sub: 'user-456' }, { expiresIn: '-10s' });
    await expect(devJwtAuthenticationStrategy.validateBearerTokenAndResolveAuthenticatedUser(expiredToken)).rejects.toThrow(
      UnauthorizedException,
    );
  });

  it('rejects a malformed token', async () => {
    await expect(
      devJwtAuthenticationStrategy.validateBearerTokenAndResolveAuthenticatedUser('not-a-valid-jwt'),
    ).rejects.toThrow(UnauthorizedException);
  });

  it('rejects a token signed with a different secret', async () => {
    const otherJwtService = new JwtService({ secret: 'a-different-secret' });
    const tokenSignedElsewhere = otherJwtService.sign({ sub: 'user-789' });
    await expect(
      devJwtAuthenticationStrategy.validateBearerTokenAndResolveAuthenticatedUser(tokenSignedElsewhere),
    ).rejects.toThrow(UnauthorizedException);
  });
});
