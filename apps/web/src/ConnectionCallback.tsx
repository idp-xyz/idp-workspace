import { useEffect, useRef, useState } from 'react';
import type { Api } from './api.ts';
import { CONNECTIONS_PATH, clearCallback, connectionError, isLocalPath } from './connections.ts';
import type { OAuthParams } from './connections.ts';

export interface ConnectionCallbackProps {
  api: Api;
  params: OAuthParams;
  /** the title of a connection, for the sentence that says it is connected (the id when unknown) */
  titleOf?(id: string): Promise<string | undefined>;
  /** where the shell goes (a path of this site); the app replaces the page's address, tests record it */
  go?(path: string): void;
}

// The page the third party's redirect lands on (ADR-0001 D30). The server holds the connect this member started; this page hands it
// what the redirect brought, once, and says in plain words how it came out - from fixed codes, never from the third party's text.
export function ConnectionCallback({ api, params, titleOf, go = (p) => window.location.assign(p) }: ConnectionCallbackProps) {
  const [state, setState] = useState<{ kind: 'working' } | { kind: 'done'; text: string; back: string } | { kind: 'failed'; text: string }>({ kind: 'working' });
  const started = useRef(false);
  useEffect(() => {
    if (started.current) return; // StrictMode runs an effect twice: the redirect is used once
    started.current = true;
    clearCallback(sessionStorage);
    (async () => {
      try {
        const r = await api.finishConnection({ code: params.code, state: params.state, iss: params.iss, error: params.error });
        const title = (await titleOf?.(r.connection).catch(() => undefined)) ?? r.connection;
        const back = isLocalPath(r.returnPath) ? r.returnPath : CONNECTIONS_PATH;
        setState({ kind: 'done', text: r.reviewer ? `评审账号已连接：${title}` : `已连接：${title}`, back });
        setTimeout(() => go(back), 1500);
      } catch (e) {
        setState({ kind: 'failed', text: connectionError(e, '连接没有完成') });
      }
    })();
  }, [api, params, titleOf, go]);
  if (state.kind === 'working') return <p className="muted" role="status">正在完成连接…</p>;
  if (state.kind === 'done') return (
    <div role="status">
      <p className="ok">{state.text}</p>
      <p className="muted">正在回到原来的页面。之后回到对话里再问一次，智能体就能用它了。</p>
      <div className="actions"><button onClick={() => go(state.back)}>现在回去</button></div>
    </div>
  );
  return (
    <div>
      <p className="error" role="alert">{state.text}</p>
      <div className="actions"><button onClick={() => go(CONNECTIONS_PATH)}>回到已连接的服务</button></div>
    </div>
  );
}
