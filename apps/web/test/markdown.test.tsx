import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/react';
import { Markdown } from '../src/agent/markdown.tsx';

describe('Markdown', () => {
  it('renders what answers use, as elements and never as HTML', () => {
    const { container } = render(<Markdown text={[
      '## Result', '', 'The value is **42** (see `v`), ~~not 41~~ and *fine*.', '', '```python', 'v = 6 * 7', 'print(v)', '```', '',
      '- first', '- second', '  - nested', '', '1. one', '2. two', '', '> a quote', '', '| a | b |', '|---|---|', '| 1 | 2 |', '', '---', 'Plain <b>text</b> stays text.',
    ].join('\n')} />);
    expect(container.querySelector('h4')?.textContent).toBe('Result');
    expect(container.querySelector('strong')?.textContent).toBe('42');
    expect(container.querySelector('p code')?.textContent).toBe('v');
    expect(container.querySelector('del')?.textContent).toBe('not 41');
    expect(container.querySelector('em')?.textContent).toBe('fine');
    expect(container.querySelector('pre code')?.textContent).toBe('v = 6 * 7\nprint(v)');
    expect(container.querySelector('.codeblock-head')?.textContent).toContain('python');
    expect(container.querySelectorAll('ul > li').length).toBe(3);
    expect(container.querySelector('ul ul li')?.textContent).toBe('nested');
    expect(container.querySelectorAll('ol > li').length).toBe(2);
    expect(container.querySelector('blockquote')?.textContent).toBe('a quote');
    expect(container.querySelectorAll('table td').length).toBe(2);
    expect(container.querySelector('hr')).not.toBeNull();
    expect(container.querySelector('b')).toBeNull(); // markup in the text is text
    expect(container.textContent).toContain('Plain <b>text</b> stays text.');
  });

  it('links only http(s), opens them apart from the shell, and keeps soft line breaks', () => {
    const { container } = render(<Markdown text={'See [docs](https://example.com/a) or https://example.org/x.\nNext line [bad](javascript:alert(1)) and [also](ftp://x).'} />);
    const links = [...container.querySelectorAll('a')];
    expect(links.map((a) => a.getAttribute('href'))).toEqual(['https://example.com/a', 'https://example.org/x']);
    for (const a of links) expect(a.getAttribute('rel')).toBe('noopener noreferrer');
    expect(container.textContent).toContain('[bad](javascript:alert(1))');
    expect(container.querySelector('br')).not.toBeNull();
  });
});
