import { describe, expect, it } from 'vitest';
import { ApiError } from '../src/api.ts';
import { friendly, runFailure } from '../src/errors.ts';

describe('friendly', () => {
  it('says what happened in the member\'s terms and never repeats a downstream address', () => {
    const refused = friendly(new ApiError(409, 'TARGET_REJECTED', 'http://idp-host.idp-host.svc.cluster.local:8450 answered 403 FORBIDDEN'), '问订单助手');
    expect(refused).toContain('问订单助手：');
    expect(refused).toContain('host-user');
    expect(refused).not.toContain('svc.cluster.local');
    const conflict = friendly(new ApiError(409, 'TARGET_REJECTED', 'http://idp-host:8450 answered 409 CONFLICT'));
    expect(conflict).toContain('另一个智能体'); // the cause the member can act on, not a guess about releases
    expect(conflict).not.toContain('idp-host:8450');
    expect(friendly(new ApiError(502, 'TARGET_UNAVAILABLE', 'http://x unreachable'))).toBe('目标系统暂时不可用，请稍后再试');
    expect(friendly(new ApiError(422, 'INVALID_INPUT', '/message: minLength'))).toBe('输入不符合要求：/message: minLength');
    expect(friendly(new ApiError(404, 'NOT_FOUND', 'no such run'))).toContain('找不到');
    expect(friendly(new ApiError(418, 'TEAPOT', 'see http://a.b/c'))).toBe('TEAPOT see 目标系统');
    expect(friendly(new Error('boom'))).toBe('boom');
  });
  it('names a run\'s end', () => {
    expect(runFailure('RUN_TIMEOUT', '')).toBe('智能体超过了运行时限');
    expect(runFailure('CANCELED', 'canceled by the owner')).toBe('已取消');
    expect(runFailure('MODEL_ERROR', 'upstream 500 at http://m/v1')).toBe('模型调用失败：upstream 500 at 目标系统');
    expect(runFailure('', '')).toBe('运行失败');
  });
});
