import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { Inbox } from '../src/Inbox.tsx';
import { orderSources, plainTitle, safeLink } from '../src/Sources.tsx';
import type { Api } from '../src/api.ts';
import type { InboxItem, RunSource } from '../src/types.ts';

const run: InboxItem = {
  itemId: 'r1', workspaceId: 'w1', producer: 'idp-workspace', externalRef: 'run:ws_42', kind: 'run', title: 'Ask the order assistant — finished',
  summary: 'Two days [1]; four [7].', ref: { system: 'host', kind: 'run', id: 'ws_42' }, priority: 'normal', actions: [], entities: [],
  status: 'open', version: 3, createdAt: '2026-11-19T09:00:00Z', updatedAt: '2026-11-19T09:05:00Z',
};
const ws = { workspaceId: 'w1', name: 'us-ops', displayName: 'US' };
const show = (item: InboxItem) => render(<Inbox api={{} as Api} ws={ws} items={[item]} apps={[]} confirm={async () => true} notify={() => {}} onChanged={() => {}} />);

const sources: RunSource[] = [
  { n: 2, title: 'Carrier notices', url: 'https://carrier.example/notices', cited: false },
  { n: 1, title: 'Customs clearance times — Port Authority', url: 'https://port.example/customs/times?x=1', fetchedAt: '2026-11-19T10:00:00Z', sha256: 'a'.repeat(64), cited: true },
];

describe('a run item\'s web sources', () => {
  it('lists number, title, host name, when it was read, whether the answer cited it, and the full address', () => {
    const { container } = show({ ...run, sources });
    expect(screen.getByText('来源（2）')).toBeTruthy();
    const items = container.querySelectorAll('.sources li');
    expect(items.length).toBe(2);
    expect(items[0].textContent).toContain('[1]'); // cited sources come first
    expect(items[0].textContent).toContain('Customs clearance times — Port Authority');
    expect(items[0].textContent).toContain('port.example');
    expect(items[0].textContent).toContain('回答引用了它');
    expect(items[0].textContent).toContain('读取于');
    expect(items[1].textContent).toContain('回答没有引用');
    expect(items[1].textContent).toContain('只搜索到，没有读取');
    const a = items[0].querySelector('a')!;
    expect(a.getAttribute('href')).toBe('https://port.example/customs/times?x=1');
    expect(a.getAttribute('target')).toBe('_blank');
    expect(a.getAttribute('rel')).toBe('noopener noreferrer');
    expect(a.textContent).toBe('https://port.example/customs/times?x=1'); // the address is on the page before it is followed
    expect(a.getAttribute('title')).toBe(a.textContent);
    expect(container.querySelector('.notice')).toBeNull(); // nothing unverified: no notice
  });

  it('names the numbers an answer cites that are no source, and only then', () => {
    const { container, rerender } = show({ ...run, sources, unverifiedCitations: [7, 9] });
    expect(container.querySelector('.notice')!.textContent).toBe('回答引用了 [7]、[9]，它们不在这次运行的来源里：请先核对，再据此行事。');
    rerender(<Inbox api={{} as Api} ws={ws} items={[{ ...run, unverifiedCitations: [7] }]} apps={[]} confirm={async () => true} notify={() => {}} onChanged={() => {}} />);
    expect(container.querySelector('.notice')!.textContent).toBe('回答引用了 [7]，它不在这次运行的来源里：请先核对，再据此行事。');
    expect(container.querySelector('.sources li')).toBeNull(); // the notice stands without a list
    rerender(<Inbox api={{} as Api} ws={ws} items={[{ ...run, sources }]} apps={[]} confirm={async () => true} notify={() => {}} onChanged={() => {}} />);
    expect(container.querySelector('.notice')).toBeNull();
  });

  it('shows nothing for a run without sources and nothing on an item that is no run', () => {
    const { container } = show(run);
    expect(container.querySelector('.sources')).toBeNull();
    const other = render(<Inbox api={{} as Api} ws={ws} items={[{ ...run, itemId: 'n1', kind: 'notification', ref: { system: 'external', kind: 'x', id: '1' }, sources }]} apps={[]} confirm={async () => true} notify={() => {}} onChanged={() => {}} />);
    expect(other.container.querySelector('.sources')).toBeNull();
  });

  it('renders a hostile title as escaped text and a javascript: address as text without a link', () => {
    const hostile: RunSource[] = [
      { n: 1, title: '<script>window.pwned=1</script>', url: 'https://a.example/1', cited: true },
      { n: 2, title: '<img src=x onerror="window.pwned=2">', url: 'https://a.example/2', cited: false },
      { n: 3, title: '[click](javascript:window.pwned=3) **bold** `code` <b>x</b>', url: 'https://a.example/3', cited: false },
      { n: 4, title: 'invoice‮fdp.exe⁦ end', url: 'https://a.example/4', cited: false },
      { n: 5, title: 'js link', url: 'javascript:window.pwned=5', cited: false },
      { n: 6, title: 'JaVaScRiPt link', url: ' JaVaScRiPt:window.pwned=6', cited: false },
      { n: 7, title: 'data link', url: 'data:text/html,<script>window.pwned=7</script>', cited: false },
      { n: 8, title: 'user info', url: 'https://trusted.example@evil.example/', cited: false },
    ];
    const { container } = show({ ...run, sources: hostile });
    const html = container.innerHTML;
    // the page's bytes: markup from a title is there only as text
    expect(html).not.toContain('<script');
    expect(html).not.toContain('<img');
    expect(html).not.toContain('<b>');
    expect(html).toContain('&lt;script&gt;window.pwned=1&lt;/script&gt;');
    expect(html).toContain('&lt;img src=x onerror="window.pwned=2"&gt;');
    expect(html).not.toMatch(/‮|⁦/);
    expect(container.querySelector('.sources script, .sources img, .sources b, .sources code, .sources strong')).toBeNull();
    // the Markdown in a title is not interpreted: it is a text node, link syntax and all
    expect(screen.getByText('[click](javascript:window.pwned=3) **bold** `code` <b>x</b>')).toBeTruthy();
    expect(screen.getByText(/invoicefdp\.exe end/)).toBeTruthy();
    const hrefs = Array.from(container.querySelectorAll('.sources a')).map((a) => a.getAttribute('href'));
    expect(hrefs.every((h) => /^https:\/\/a\.example\/[1-4]$/.test(h!))).toBe(true);
    expect(hrefs.length).toBe(4);
    expect(html.toLowerCase()).not.toMatch(/href="\s*javascript:/);
    expect(html.toLowerCase()).not.toMatch(/href="data:/);
    expect((window as unknown as { pwned?: number }).pwned).toBeUndefined();
  });

  it('follows only http and https addresses that parse and carry no user information', () => {
    for (const bad of ['javascript:alert(1)', 'JAVASCRIPT:alert(1)', 'data:text/html,x', 'vbscript:x', 'ftp://a.example/', 'https://u:p@a.example/', 'https://a.example/a b', 'https://a.example/‮', '//a.example/', 'a.example', '', 'https:///x', 'https://a.example/' + 'a'.repeat(2048)]) {
      expect(safeLink(bad), bad).toBeNull();
    }
    expect(safeLink('HTTPS://A.EXAMPLE/x')!.href).toBe('https://a.example/x');
    expect(safeLink('https://bücher.example/')!.hostname).toBe('xn--bcher-kva.example'); // a look-alike host is shown as it will be resolved
    expect(plainTitle('  a​ \n b‮ ')).toBe('a b');
    expect(orderSources([{ n: 3, title: '', url: 'https://x.example/', cited: false }, { n: 5, title: '', url: 'https://x.example/', cited: true }, { n: 1, title: '', url: 'https://x.example/', cited: false }]).map((s) => s.n)).toEqual([5, 1, 3]);
  });
});
