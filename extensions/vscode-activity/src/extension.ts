// The sbx activity extension. code-server starts it in every workspace;
// it posts to the agent's loopback activity endpoint named by
// SBX_ACTIVITY_URL, and does nothing elsewhere.
import * as vscode from "vscode";
import { Reporter, type Report } from "./reporter.ts";

export function activate(context: vscode.ExtensionContext): void {
  const url = process.env.SBX_ACTIVITY_URL;
  if (!url) {
    return;
  }
  const send = (r: Report): void => {
    fetch(url, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(r) }).catch(() => {
      // The agent may be frozen for a snapshot; activity is best effort.
    });
  };
  const reporter = new Reporter(send);
  const timer = setInterval(() => reporter.reminder(), 30_000);
  context.subscriptions.push(
    { dispose: () => clearInterval(timer) },
    vscode.workspace.onDidChangeTextDocument((e) => {
      if (e.document.uri.scheme === "file" && e.contentChanges.length > 0) {
        reporter.edit();
      }
    }),
    vscode.workspace.onDidSaveTextDocument(() => reporter.save()),
    vscode.window.onDidStartTerminalShellExecution((e) => reporter.commandStarted(e.execution, e.execution.commandLine.value)),
    vscode.window.onDidEndTerminalShellExecution((e) => reporter.commandEnded(e.execution)),
  );
}

export function deactivate(): void {}
