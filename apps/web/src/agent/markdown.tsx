import { useState } from 'react';
import type { ReactNode } from 'react';

// The Markdown an agent writes, rendered as React elements - never as HTML strings, so nothing the model
// or a tool returned can become script or markup (the shell's CSP forbids inline script anyway). Covers
// what answers use: paragraphs, headings, lists, quotes, fenced code (with a copy button), tables, rules,
// and inline code, emphasis, strikethrough and http(s) links.

export function Markdown({ text }: { text: string }) {
  return <div className="md">{renderBlocks(text)}</div>;
}

const FENCE = /^\s*(```|~~~)\s*([\w+-]*)\s*$/;
const HEADING = /^(#{1,6})\s+(.+?)\s*#*\s*$/;
const RULE = /^\s*([-*_])(\s*\1){2,}\s*$/;
const ITEM = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/;
const QUOTE = /^\s*>\s?(.*)$/;
const TABLE_SEP = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;

export function renderBlocks(text: string): ReactNode[] {
  const lines = text.replace(/\r\n?/g, '\n').split('\n');
  const out: ReactNode[] = [];
  let i = 0;
  let key = 0;
  const k = () => `b${key++}`;
  while (i < lines.length) {
    const line = lines[i];
    if (line.trim() === '') { i++; continue; }
    const fence = FENCE.exec(line);
    if (fence) {
      const code: string[] = [];
      i++;
      while (i < lines.length && !(lines[i].trim().startsWith(fence[1]) && lines[i].trim().length <= fence[1].length + 1)) code.push(lines[i++]);
      i++; // the closing fence (or the end)
      out.push(<CodeBlock key={k()} lang={fence[2]} code={code.join('\n')} />);
      continue;
    }
    const h = HEADING.exec(line);
    if (h) {
      const level = Math.min(h[1].length + 2, 6); // an answer's title is a section of the page, not of the document
      out.push(createHeading(level, k(), inline(h[2])));
      i++;
      continue;
    }
    if (RULE.test(line)) { out.push(<hr key={k()} />); i++; continue; }
    if (QUOTE.test(line)) {
      const q: string[] = [];
      while (i < lines.length && QUOTE.test(lines[i])) q.push(QUOTE.exec(lines[i++])![1]);
      out.push(<blockquote key={k()}>{renderBlocks(q.join('\n'))}</blockquote>);
      continue;
    }
    if (ITEM.test(line)) {
      const items: { indent: number; ordered: boolean; text: string }[] = [];
      while (i < lines.length && (ITEM.test(lines[i]) || (items.length && /^\s{2,}\S/.test(lines[i]) && !FENCE.test(lines[i])))) {
        const m = ITEM.exec(lines[i]);
        if (m) items.push({ indent: m[1].length, ordered: /\d/.test(m[2]), text: m[3] });
        else items[items.length - 1].text += '\n' + lines[i].trim(); // a continuation line
        i++;
      }
      out.push(renderList(items, 0, k));
      continue;
    }
    if (line.includes('|') && i + 1 < lines.length && TABLE_SEP.test(lines[i + 1])) {
      const rows: string[][] = [cells(line)];
      i += 2;
      while (i < lines.length && lines[i].includes('|') && lines[i].trim() !== '') rows.push(cells(lines[i++]));
      out.push(
        <table key={k()}>
          <thead><tr>{rows[0].map((c, j) => <th key={j}>{inline(c)}</th>)}</tr></thead>
          <tbody>{rows.slice(1).map((r, ri) => <tr key={ri}>{r.map((c, j) => <td key={j}>{inline(c)}</td>)}</tr>)}</tbody>
        </table>,
      );
      continue;
    }
    const p: string[] = [];
    while (i < lines.length && lines[i].trim() !== '' && !FENCE.test(lines[i]) && !HEADING.test(lines[i]) && !ITEM.test(lines[i]) && !QUOTE.test(lines[i]) && !RULE.test(lines[i])) p.push(lines[i++]);
    out.push(<p key={k()}>{p.flatMap((l, j) => (j ? [<br key={`br${j}`} />, ...inline(l)] : inline(l)))}</p>);
  }
  return out;
}

function createHeading(level: number, key: string, children: ReactNode[]) {
  switch (level) {
    case 3: return <h3 key={key}>{children}</h3>;
    case 4: return <h4 key={key}>{children}</h4>;
    case 5: return <h5 key={key}>{children}</h5>;
    default: return <h6 key={key}>{children}</h6>;
  }
}

function cells(row: string): string[] {
  const t = row.trim().replace(/^\|/, '').replace(/\|$/, '');
  return t.split(/(?<!\\)\|/).map((c) => c.trim().replace(/\\\|/g, '|'));
}

function renderList(items: { indent: number; ordered: boolean; text: string }[], from: number, k: () => string): ReactNode {
  const base = items[from].indent;
  const ordered = items[from].ordered;
  const children: ReactNode[] = [];
  let i = from;
  while (i < items.length && items[i].indent >= base) {
    const item = items[i];
    const nested: typeof items = [];
    let j = i + 1;
    while (j < items.length && items[j].indent > base) nested.push(items[j++]);
    children.push(
      <li key={k()}>
        {inline(item.text)}
        {nested.length > 0 && renderList(nested, 0, k)}
      </li>,
    );
    i = j;
  }
  return ordered ? <ol key={k()}>{children}</ol> : <ul key={k()}>{children}</ul>;
}

const INLINE = /(`[^`\n]+`|\*\*[^*\n]+\*\*|__[^_\n]+__|~~[^~\n]+~~|\*[^*\s][^*\n]*\*|\[[^\]\n]+\]\(https?:\/\/[^)\s]+\)|https?:\/\/[^\s<>()]+[^\s<>().,;:!?'"])/g;

/** inline Markdown of one line, as React nodes (text nodes carry the model's words verbatim) */
export function inline(text: string): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  let n = 0;
  for (const m of text.matchAll(INLINE)) {
    const s = m[0];
    const at = m.index ?? 0;
    if (at > last) out.push(text.slice(last, at));
    last = at + s.length;
    const key = `i${n++}`;
    if (s.startsWith('`')) out.push(<code key={key}>{s.slice(1, -1)}</code>);
    else if (s.startsWith('**') || s.startsWith('__')) out.push(<strong key={key}>{inline(s.slice(2, -2))}</strong>);
    else if (s.startsWith('~~')) out.push(<del key={key}>{inline(s.slice(2, -2))}</del>);
    else if (s.startsWith('*')) out.push(<em key={key}>{inline(s.slice(1, -1))}</em>);
    else if (s.startsWith('[')) {
      const close = s.indexOf('](');
      out.push(<a key={key} href={s.slice(close + 2, -1)} target="_blank" rel="noopener noreferrer">{inline(s.slice(1, close))}</a>);
    } else out.push(<a key={key} href={s} target="_blank" rel="noopener noreferrer">{s}</a>);
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

export function CodeBlock({ lang, code }: { lang?: string; code: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch { /* no clipboard: the text is still selectable */ }
  };
  return (
    <div className="codeblock">
      <div className="codeblock-head">
        <span className="mono">{lang || 'text'}</span>
        <button type="button" className="ghost" onClick={copy} aria-label="复制代码">{copied ? '已复制' : '复制'}</button>
      </div>
      <pre><code>{code}</code></pre>
    </div>
  );
}
