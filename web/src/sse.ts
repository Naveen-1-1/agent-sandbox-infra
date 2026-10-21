// Server-sent events over fetch. EventSource can't send an Authorization
// header, so the stream is read with fetch and parsed here, following the
// WHATWG event stream format.

export interface ServerEvent {
  event: string;
  data: string;
  id?: string;
}

// SSEParser turns chunks of an event stream into events. Lines may end in
// \n, \r\n or \r, and chunks may split anywhere, a \r\n pair included.
export class SSEParser {
  private buf = "";
  private event = "";
  private data: string[] = [];
  private id: string | undefined;
  private skipLF = false; // the last chunk ended in \r, maybe half of \r\n

  feed(chunk: string): ServerEvent[] {
    if (this.skipLF && chunk) {
      if (chunk.startsWith("\n")) chunk = chunk.slice(1);
      this.skipLF = false;
    }
    this.buf += chunk;
    const out: ServerEvent[] = [];
    for (;;) {
      const m = /\r\n|\n|\r/.exec(this.buf);
      if (!m) break;
      if (m[0] === "\r" && m.index === this.buf.length - 1) this.skipLF = true;
      const line = this.buf.slice(0, m.index);
      this.buf = this.buf.slice(m.index + m[0].length);
      const ev = this.line(line);
      if (ev) out.push(ev);
    }
    return out;
  }

  private line(line: string): ServerEvent | undefined {
    if (line === "") {
      const ev = this.data.length > 0 ? { event: this.event || "message", data: this.data.join("\n"), id: this.id } : undefined;
      this.event = "";
      this.data = [];
      return ev;
    }
    if (line.startsWith(":")) return undefined; // a comment: keep-alive
    const colon = line.indexOf(":");
    const field = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    switch (field) {
      case "event":
        this.event = value;
        break;
      case "data":
        this.data.push(value);
        break;
      case "id":
        this.id = value;
        break;
    }
    return undefined;
  }
}

// readEvents reads one event stream until it ends. Aborting the fetch that
// opened it rejects with an AbortError.
export async function readEvents(
  body: ReadableStream<Uint8Array>,
  onEvent: (ev: ServerEvent) => void,
): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  const parser = new SSEParser();
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      for (const ev of parser.feed(decoder.decode(value, { stream: true }))) onEvent(ev);
    }
  } finally {
    reader.releaseLock();
  }
}

// backoff is the wait before reconnect attempt n (from 0): doubling from
// half a second to 15 seconds.
export function backoff(n: number): number {
  return Math.min(500 * 2 ** n, 15_000);
}
