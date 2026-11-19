import type { RunSource } from './types.ts';

// The web sources of a finished run (ADR-0001 D29). A title and an address come from a web page, so nothing here
// is trusted: React renders every string as text (never as HTML or Markdown), the title is cleaned again of control
// and bidirectional characters, and an address becomes a link only when it parses and its scheme is http or https.
// The shell's CSP forbids inline script, and this adds none.

const HIDDEN = /[\p{Cc}\p{Cf}\u2028\u2029]/gu;
const HAS_HIDDEN = /[\p{Cc}\p{Cf}\u2028\u2029]/u;

// scheme, two slashes, then the first character of a host (not another slash, a backslash, a query or a fragment)
const EXPLICIT_HOST = /^https?:\/\/[^/\\?#]/i;

/** a title as plain text: no control or format (bidi) characters, whitespace collapsed */
export function plainTitle(title: string): string {
  return title.replace(/\s+/g, ' ').replace(HIDDEN, '').trim();
}

/** the address when it may be followed (http or https, a host, no user information), else null */
export function safeLink(raw: string): URL | null {
  if (raw.length > 2048 || HAS_HIDDEN.test(raw) || /\s/.test(raw) || !EXPLICIT_HOST.test(raw)) return null; // the browser's parser repairs "https:///x" and "https:\\x": the text must name its host plainly
  try {
    const u = new URL(raw);
    return (u.protocol === 'http:' || u.protocol === 'https:') && u.hostname !== '' && u.username === '' && u.password === '' ? u : null;
  } catch {
    return null;
  }
}

/** cited sources first, each group in the order of its numbers */
export function orderSources(sources: RunSource[]): RunSource[] {
  return [...sources].sort((a, b) => Number(b.cited) - Number(a.cited) || a.n - b.n);
}

export function RunSources({ sources, unverified }: { sources?: RunSource[]; unverified?: number[] }) {
  const nums = (unverified ?? []).filter((n) => Number.isInteger(n) && n > 0);
  if (!(sources?.length) && nums.length === 0) return null;
  return (
    <div className="sources">
      {nums.length > 0 && (
        <p className="notice" role="note">
          回答引用了 {nums.map((n) => `[${n}]`).join('、')}，{nums.length > 1 ? '它们不在' : '它不在'}这次运行的来源里：请先核对，再据此行事。
        </p>
      )}
      {!!sources?.length && (
        <details open={sources.length <= 5}>
          <summary>来源（{sources.length}）</summary>
          <ol>
            {orderSources(sources).map((s) => {
              const link = safeLink(s.url);
              const read = s.fetchedAt ? new Date(s.fetchedAt) : null;
              return (
                <li key={s.n} className={s.cited ? 'cited' : 'uncited'}>
                  <span className="src-n">[{s.n}]</span>{' '}
                  <span className="src-title">{plainTitle(s.title) || '（无标题）'}</span>{' '}
                  <span className="src-host">{link ? link.hostname : '（地址不可打开）'}</span>{' '}
                  <span className={`state ${s.cited ? 'done' : ''}`}>{s.cited ? '回答引用了它' : '回答没有引用'}</span>{' '}
                  <span className="src-read">{read && !Number.isNaN(read.getTime()) ? `读取于 ${read.toLocaleString()}` : '只搜索到，没有读取'}</span>
                  <div className="src-url">
                    {link
                      ? <a href={link.href} target="_blank" rel="noopener noreferrer" title={link.href}>{link.href}</a>
                      : <span title="这个地址不是 http 或 https，不提供链接">{s.url}</span>}
                  </div>
                </li>
              );
            })}
          </ol>
        </details>
      )}
    </div>
  );
}
