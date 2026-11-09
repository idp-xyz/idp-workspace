import { ApiError } from './api.ts';

// What the member reads when a call fails: the cause in their terms, never a downstream address. The
// codes are the workspace's (ADR-0001 D4: INVALID_INPUT, TARGET_REJECTED, TARGET_UNAVAILABLE) and the
// relayed ones of idp-host (D22).

const noAddress = (s: string) => s.replace(/https?:\/\/[^\s)]+/g, '目标系统').replace(/\s+/g, ' ').trim();

export function friendly(e: unknown, what = ''): string {
  const prefix = what ? `${what}：` : '';
  if (!(e instanceof ApiError)) return prefix + noAddress(String(e instanceof Error ? e.message : e));
  const m = e.message;
  switch (e.code) {
    case 'TARGET_REJECTED':
      if (/403|FORBIDDEN|DELEGATION_REFUSED/.test(m)) return `${prefix}目标系统拒绝了你：你没有运行这个智能体的权限（需要 host-user 角色），请联系管理员`;
      if (/409|CONFLICT/.test(m)) return `${prefix}目标系统拒绝：状态冲突（智能体可能没有已发布的版本）`;
      if (/404|NOT_FOUND/.test(m)) return `${prefix}目标系统里没有这个智能体`;
      return `${prefix}目标系统拒绝了这次调用（${noAddress(m)}）`;
    case 'TARGET_UNAVAILABLE':
      return `${prefix}目标系统暂时不可用，请稍后再试`;
    case 'INVALID_INPUT':
      return `${prefix}输入不符合要求：${noAddress(m)}`;
    case 'INVALID':
      return `${prefix}请求无效：${noAddress(m)}`;
    case 'NOT_FOUND':
      return `${prefix}找不到对象，它可能已被删除或不属于你`;
    case 'FORBIDDEN':
      return `${prefix}你没有权限执行这个操作`;
    case 'CONFLICT':
      return `${prefix}状态已改变：${noAddress(m)}`;
    case 'UNAUTHENTICATED':
      return `${prefix}登录已失效，请重新登录`;
    default:
      return `${prefix}${e.code} ${noAddress(m)}`.trim();
  }
}

/** a run's failure as idp-host names it (RUN_ERROR / failureCode), in the member's words */
export function runFailure(code: string, message: string): string {
  switch (code) {
    case 'RUN_TIMEOUT': return '智能体超过了运行时限';
    case 'CANCELED': return '已取消';
    case 'MODEL_ERROR': return `模型调用失败${message ? '：' + noAddress(message) : ''}`;
    case 'CONTEXT_UNAVAILABLE': return '取不到企业知识，运行被拒绝';
    case 'GUARD_BLOCKED': return '输出被内容护栏拦下';
    default: return [code, noAddress(message)].filter(Boolean).join(' ') || '运行失败';
  }
}
