/**
 * @file internalServiceAuthGuard.spec.ts
 * Unit tests for {@link InternalServiceAuthGuard}: valid shared secret is permitted,
 * missing/incorrect shared secret is rejected with UnauthorizedException.
 */

import { ExecutionContext, UnauthorizedException } from '@nestjs/common';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import { InternalServiceAuthGuard, INTERNAL_SERVICE_SHARED_SECRET_HEADER_NAME } from './internalServiceAuthGuard';

function buildHttpExecutionContext(headers: Record<string, string | string[] | undefined>): ExecutionContext {
  return {
    switchToHttp: () => ({ getRequest: () => ({ headers }) }),
  } as unknown as ExecutionContext;
}

describe('InternalServiceAuthGuard', () => {
  const testSharedSecret = 'unit-test-internal-shared-secret';

  beforeEach(() => {
    process.env.INTERNAL_SERVICE_SHARED_SECRET = testSharedSecret;
  });

  afterEach(() => {
    delete process.env.INTERNAL_SERVICE_SHARED_SECRET;
  });

  it('permits a request presenting the correct shared secret header', () => {
    const guard = new InternalServiceAuthGuard(new ApplicationConfigurationService());
    const context = buildHttpExecutionContext({ [INTERNAL_SERVICE_SHARED_SECRET_HEADER_NAME]: testSharedSecret });
    expect(guard.canActivate(context)).toBe(true);
  });

  it('rejects a request with no shared secret header at all', () => {
    const guard = new InternalServiceAuthGuard(new ApplicationConfigurationService());
    const context = buildHttpExecutionContext({});
    expect(() => guard.canActivate(context)).toThrow(UnauthorizedException);
  });

  it('rejects a request presenting an incorrect shared secret', () => {
    const guard = new InternalServiceAuthGuard(new ApplicationConfigurationService());
    const context = buildHttpExecutionContext({ [INTERNAL_SERVICE_SHARED_SECRET_HEADER_NAME]: 'wrong-secret' });
    expect(() => guard.canActivate(context)).toThrow(UnauthorizedException);
  });

  it('rejects a request presenting an empty-string shared secret', () => {
    const guard = new InternalServiceAuthGuard(new ApplicationConfigurationService());
    const context = buildHttpExecutionContext({ [INTERNAL_SERVICE_SHARED_SECRET_HEADER_NAME]: '' });
    expect(() => guard.canActivate(context)).toThrow(UnauthorizedException);
  });
});
