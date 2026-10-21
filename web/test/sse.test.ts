import { describe, expect, it } from "vitest";

import { backoff, readEvents, SSEParser, type ServerEvent } from "../src/sse.ts";

describe("SSEParser", () => {
  it("parses events the way the gateway writes them", () => {
    const p = new SSEParser();
    expect(p.feed('event: workspace\ndata: {"name":"a"}\n\nevent: ready\ndata: {}\n\n')).toEqual([
      { event: "workspace", data: '{"name":"a"}', id: undefined },
      { event: "ready", data: "{}", id: undefined },
    ]);
  });

  it("keeps partial events across chunks, wherever they split", () => {
    const stream = 'event: workspace\r\ndata: {"a":1}\r\n\r\n: ping\r\n\r\nevent: deleted\rdata: x\r\r';
    for (let i = 0; i <= stream.length; i++) {
      const p = new SSEParser();
      const got = [...p.feed(stream.slice(0, i)), ...p.feed(stream.slice(i))];
      expect(got.map((e) => [e.event, e.data]), `split at ${i}`).toEqual([
        ["workspace", '{"a":1}'],
        ["deleted", "x"],
      ]);
    }
  });

  it("joins data lines, defaults the type and skips comments and empty events", () => {
    const p = new SSEParser();
    expect(p.feed(": ping\n\ndata: one\ndata:two\nid: 7\nretry: 10\n\nevent: nothing\n\n")).toEqual([
      { event: "message", data: "one\ntwo", id: "7" },
    ]);
  });

  it("holds a line until it ends", () => {
    const p = new SSEParser();
    expect(p.feed("data: no end yet")).toEqual([]);
    expect(p.feed("\n\n")).toEqual([{ event: "message", data: "no end yet", id: undefined }]);
  });
});

describe("readEvents", () => {
  it("decodes a byte stream split inside a character", async () => {
    const bytes = new TextEncoder().encode('event: workspace\ndata: {"name":"ü"}\n\n');
    const cut = bytes.indexOf(0xc3) + 1; // between the two bytes of ü
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(bytes.slice(0, cut));
        c.enqueue(bytes.slice(cut));
        c.close();
      },
    });
    const got: ServerEvent[] = [];
    await readEvents(body, (e) => got.push(e));
    expect(got).toEqual([{ event: "workspace", data: '{"name":"ü"}', id: undefined }]);
  });
});

describe("backoff", () => {
  it("doubles up to a cap", () => {
    expect([0, 1, 2, 3, 10].map(backoff)).toEqual([500, 1000, 2000, 4000, 15_000]);
  });
});
