// Reporter turns editor events into activity reports for the sbx agent.
// It reports only real input: edits (at most one every few seconds),
// saves, and terminal commands starting and ending. An open editor that
// nobody types in reports nothing, so it never keeps a workspace awake.
// While a terminal command runs, a periodic reminder lists it, so a command
// whose editor went away stops counting after a while.

export type Report =
  | { kind: "edit" }
  | { kind: "save" }
  | { kind: "terminal-start"; id: string; command: string }
  | { kind: "terminal-end"; id: string }
  | { kind: "terminal-running"; ids: string[] };

export class Reporter {
  private readonly send: (r: Report) => void;
  private readonly now: () => number;
  private readonly editEvery: number;
  private readonly prefix: string;
  private lastEdit = -Infinity;
  private nextId = 0;
  private readonly running = new Map<object, string>();

  constructor(send: (r: Report) => void, now: () => number = Date.now, editEvery = 5000, prefix = String(process.pid)) {
    this.send = send;
    this.now = now;
    this.editEvery = editEvery;
    this.prefix = prefix;
  }

  edit(): void {
    const t = this.now();
    if (t - this.lastEdit < this.editEvery) {
      return;
    }
    this.lastEdit = t;
    this.send({ kind: "edit" });
  }

  save(): void {
    this.send({ kind: "save" });
  }

  // execution identifies one command run in a terminal.
  commandStarted(execution: object, command: string): void {
    const id = `${this.prefix}-${++this.nextId}`;
    this.running.set(execution, id);
    this.send({ kind: "terminal-start", id, command });
  }

  commandEnded(execution: object): void {
    const id = this.running.get(execution);
    if (id === undefined) {
      return;
    }
    this.running.delete(execution);
    this.send({ kind: "terminal-end", id });
  }

  // reminder lists the commands still running, if any.
  reminder(): void {
    if (this.running.size > 0) {
      this.send({ kind: "terminal-running", ids: [...this.running.values()] });
    }
  }
}
