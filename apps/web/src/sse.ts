// Incremental server-sent events: feed it text as it arrives, get whole events back. The shell reads
// the inbox stream with fetch (an EventSource cannot send the Authorization header, ADR-0001 D10).
export interface SSEEvent { id: string; event: string; data: string }

export function sseParser(): (chunk: string) => SSEEvent[] {
  let buffer = '';
  let id = '';
  let event = '';
  let data: string[] = [];
  return (chunk) => {
    buffer += chunk;
    const out: SSEEvent[] = [];
    let nl: number;
    while ((nl = buffer.indexOf('\n')) >= 0) {
      const line = buffer.slice(0, nl).replace(/\r$/, '');
      buffer = buffer.slice(nl + 1);
      if (line === '') {
        if (data.length) out.push({ id, event: event || 'message', data: data.join('\n') });
        event = '';
        data = [];
      } else if (line.startsWith(':')) {
        // comment / keep-alive
      } else if (line.startsWith('data:')) {
        data.push(line.slice(5).replace(/^ /, ''));
      } else if (line.startsWith('event:')) {
        event = line.slice(6).trim();
      } else if (line.startsWith('id:')) {
        id = line.slice(3).trim();
      }
    }
    return out;
  };
}
