import * as vscode from "vscode";

export function activate(context: vscode.ExtensionContext) {
  const command = vscode.commands.registerCommand("codebeam.searchSelection", async () => {
    const editor = vscode.window.activeTextEditor;
    const selection = editor?.document.getText(editor.selection).trim();

    if (!selection) {
      await vscode.window.showWarningMessage("Select text before searching Codebeam.");
      return;
    }

    const configuredBaseUrl = vscode.workspace
      .getConfiguration("codebeam")
      .get<string>("baseUrl", "http://localhost:8080");
    const baseUrl = configuredBaseUrl.replace(/\/+$/, "");
    const target = vscode.Uri.parse(`${baseUrl}/search?q=${encodeURIComponent(selection)}`);
    await vscode.env.openExternal(target);
  });

  context.subscriptions.push(command);
}

export function deactivate() {}
