// @vitest-environment node
import { describe, expect, it } from 'vitest';
import { sseParser } from '../src/sse.ts';

describe('sseParser', () => {
  it('assembles events across arbitrary chunk boundaries', () => {
    const parse = sseParser();
    const text = ': keep-alive\n\nid: 17:a\nevent: item\ndata: {"itemId":"a"}\n\nid: 18:b\r\nevent: item\r\ndata: {"itemId":\r\ndata: "b"}\r\n\r\n';
    const out = [];
    for (let i = 0; i < text.length; i += 7) out.push(...parse(text.slice(i, i + 7)));
    expect(out).toEqual([
      { id: '17:a', event: 'item', data: '{"itemId":"a"}' },
      { id: '18:b', event: 'item', data: '{"itemId":\n"b"}' },
    ]);
  });
});
