/**
 * @file mutualTlsClientCertificateGuard.spec.ts
 * Unit tests for {@link MutualTlsClientCertificateGuard}: disabled-by-default no-op,
 * enforcement allowing a properly-verified header through, and rejection of missing/
 * incorrect headers when enforcement is enabled.
 */

import { ExecutionContext, ForbiddenException } from '@nestjs/common';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import { MutualTlsClientCertificateGuard } from './mutualTlsClientCertificateGuard';

function buildHttpExecutionContext(headers: Record<string, string | string[] | undefined>): ExecutionContext {
  return {
    switchToHttp: () => ({ getRequest: () => ({ headers }) }),
    getType: () => 'http',
    getArgs: () => [],
    getArgByIndex: () => undefined,
    switchToRpc: () => ({}) as never,
    switchToWs: () => ({}) as never,
    getClass: () => ({}) as never,
    getHandler: () => ({}) as never,
  } as unknown as ExecutionContext;
}

describe('MutualTlsClientCertificateGuard', () => {
  it('allows requests through when mTLS enforcement is disabled (the default)', () => {
    delete process.env.MUTUAL_TLS_ENFORCEMENT_ENABLED;
    const guard = new MutualTlsClientCertificateGuard(new ApplicationConfigurationService());
    const context = buildHttpExecutionContext({});
    expect(guard.canActivate(context)).toBe(true);
  });

  it('allows requests through when enforcement is enabled and the header reports SUCCESS', () => {
    process.env.MUTUAL_TLS_ENFORCEMENT_ENABLED = 'true';
    const guard = new MutualTlsClientCertificateGuard(new ApplicationConfigurationService());
    const context = buildHttpExecutionContext({ 'x-client-cert-verified': 'SUCCESS' });
    expect(guard.canActivate(context)).toBe(true);
    delete process.env.MUTUAL_TLS_ENFORCEMENT_ENABLED;
  });

  it('rejects requests when enforcement is enabled and the header is missing', () => {
    process.env.MUTUAL_TLS_ENFORCEMENT_ENABLED = 'true';
    const guard = new MutualTlsClientCertificateGuard(new ApplicationConfigurationService());
    const context = buildHttpExecutionContext({});
    expect(() => guard.canActivate(context)).toThrow(ForbiddenException);
    delete process.env.MUTUAL_TLS_ENFORCEMENT_ENABLED;
  });

  it('rejects requests when enforcement is enabled and the header reports a non-SUCCESS value', () => {
    process.env.MUTUAL_TLS_ENFORCEMENT_ENABLED = 'true';
    const guard = new MutualTlsClientCertificateGuard(new ApplicationConfigurationService());
    const context = buildHttpExecutionContext({ 'x-client-cert-verified': 'FAILED' });
    expect(() => guard.canActivate(context)).toThrow(ForbiddenException);
    delete process.env.MUTUAL_TLS_ENFORCEMENT_ENABLED;
  });
});
