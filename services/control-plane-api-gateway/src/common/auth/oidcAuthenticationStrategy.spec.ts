/**
 * @file oidcAuthenticationStrategy.spec.ts
 * Unit tests for {@link OidcAuthenticationStrategy}: successful claims mapping via a
 * fake {@link OidcRelyingPartyClientLike}, verification failure propagation, and the
 * fail-safe (fail-closed) behaviour of {@link UnimplementedOidcRelyingPartyClient} when
 * no real OIDC client has been wired up. None of these tests perform any network I/O.
 */

import { UnauthorizedException } from '@nestjs/common';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import { OidcAuthenticationStrategy, UnimplementedOidcRelyingPartyClient } from './oidcAuthenticationStrategy';
import { OidcRelyingPartyClientLike, OidcResolvedIdentityClaims } from './oidcRelyingPartyClientLike.interface';

/** Deterministic in-memory fake standing in for a real openid-client-backed implementation. */
class FakeOidcRelyingPartyClient implements OidcRelyingPartyClientLike {
  public constructor(private readonly claimsByToken: Record<string, OidcResolvedIdentityClaims | undefined>) {}

  public async resolveIdentityClaimsForBearerToken(bearerTokenValue: string): Promise<OidcResolvedIdentityClaims> {
    const claims = this.claimsByToken[bearerTokenValue];
    if (!claims) {
      throw new Error('token rejected by identity provider');
    }
    return claims;
  }
}

describe('OidcAuthenticationStrategy', () => {
  let applicationConfigurationService: ApplicationConfigurationService;

  beforeEach(() => {
    applicationConfigurationService = new ApplicationConfigurationService();
  });

  it('reports its strategy identifier as "oidc"', () => {
    const strategy = new OidcAuthenticationStrategy(new UnimplementedOidcRelyingPartyClient(), applicationConfigurationService);
    expect(strategy.strategyIdentifier).toBe('oidc');
  });

  it('maps resolved OIDC claims to an AuthenticatedUserPayload on success', async () => {
    const fakeClient = new FakeOidcRelyingPartyClient({
      'valid-token': {
        subjectIdentifier: 'idp-subject-abc',
        emailAddress: 'person@example.com',
        roles: ['viewer'],
        expiresAtEpochSeconds: 1_999_999_999,
      },
    });
    const strategy = new OidcAuthenticationStrategy(fakeClient, applicationConfigurationService);

    const result = await strategy.validateBearerTokenAndResolveAuthenticatedUser('valid-token');

    expect(result).toEqual({
      subjectUserIdentifier: 'idp-subject-abc',
      displayNameOrEmail: 'person@example.com',
      grantedRoles: ['viewer'],
      tokenExpiresAtEpochSeconds: 1_999_999_999,
    });
  });

  it('defaults roles to an empty array when the IdP omits a roles claim', async () => {
    const fakeClient = new FakeOidcRelyingPartyClient({
      'no-roles-token': { subjectIdentifier: 'idp-subject-xyz', expiresAtEpochSeconds: 1_999_999_999 },
    });
    const strategy = new OidcAuthenticationStrategy(fakeClient, applicationConfigurationService);

    const result = await strategy.validateBearerTokenAndResolveAuthenticatedUser('no-roles-token');
    expect(result.grantedRoles).toEqual([]);
  });

  it('wraps identity-provider verification failures in an UnauthorizedException', async () => {
    const fakeClient = new FakeOidcRelyingPartyClient({});
    const strategy = new OidcAuthenticationStrategy(fakeClient, applicationConfigurationService);

    await expect(strategy.validateBearerTokenAndResolveAuthenticatedUser('unknown-token')).rejects.toThrow(
      UnauthorizedException,
    );
  });

  it('fails closed via UnimplementedOidcRelyingPartyClient when no real OIDC client is wired up', async () => {
    const strategy = new OidcAuthenticationStrategy(new UnimplementedOidcRelyingPartyClient(), applicationConfigurationService);

    await expect(strategy.validateBearerTokenAndResolveAuthenticatedUser('anything')).rejects.toThrow(UnauthorizedException);
  });
});
