// @ts-check
'use strict';

// This extension runs on the computer of the user, also in a
// Remote-SSH window ("extensionKind": ["ui"] in package.json). Thus it
// uses the git credentials of this computer, as the desktop client of
// webdiff does. It runs "webdiff github", which reaches the checkout on
// the remote host with ssh.

const vscode = require('vscode');
const { execFile } = require('child_process');
const { sshTarget, fetchMessage } = require('./target');

/** @param {vscode.ExtensionContext} context */
function activate(context) {
  context.subscriptions.push(
    vscode.commands.registerCommand('webdiff.sendToGitHub', () => report(sendToGitHub())),
    vscode.commands.registerCommand('webdiff.fetchFromGitHub', () => report(fetchFromGitHub())),
  );
}

function deactivate() {}

/** @param {Promise<void>} p */
async function report(p) {
  try {
    await p;
  } catch (e) {
    if (e instanceof Cancelled) {
      return;
    }
    vscode.window.showErrorMessage(e instanceof Error ? e.message : String(e));
  }
}

class Cancelled extends Error {}

/**
 * pickTarget returns the "<host>:<path>" of the workspace folder to
 * use. It prefers the folder of the active editor.
 *
 * @returns {Promise<string>}
 */
async function pickTarget() {
  const folders = (vscode.workspace.workspaceFolders || []).filter((f) => sshTarget(f.uri) !== null);
  if (folders.length === 0) {
    throw new Error('Open a folder in a Remote-SSH window first.');
  }
  let folder;
  const active = vscode.window.activeTextEditor;
  if (active) {
    const f = vscode.workspace.getWorkspaceFolder(active.document.uri);
    if (f && folders.includes(f)) {
      folder = f;
    }
  }
  if (!folder && folders.length === 1) {
    folder = folders[0];
  }
  if (!folder) {
    folder = await vscode.window.showWorkspaceFolderPick({
      placeHolder: 'Checkout',
    });
    if (!folder) {
      throw new Cancelled();
    }
  }
  const target = sshTarget(folder.uri);
  if (!target) {
    throw new Error(`${folder.name} is not in a Remote-SSH window.`);
  }
  return target;
}

/**
 * webdiff runs "webdiff github <args>" and returns the JSON output.
 * It shows a notification with title while it runs. The cancel button
 * stops the program.
 *
 * @param {string} title
 * @param {string[]} args
 * @returns {Thenable<any>}
 */
function webdiff(title, args) {
  const cfg = vscode.workspace.getConfiguration('webdiff');
  const program = cfg.get('path', 'webdiff') || 'webdiff';
  const githubURL = cfg.get('githubURL', '');
  const sub = args[0];
  const rest = args.slice(1);
  if (githubURL) {
    rest.unshift('-github-url', githubURL);
  }
  return vscode.window.withProgress(
    {
      location: vscode.ProgressLocation.Notification,
      title,
      cancellable: true,
    },
    (_progress, token) =>
      new Promise((resolve, reject) => {
        const child = execFile(
          program,
          ['github', sub, ...rest],
          {
            maxBuffer: 16 << 20,
          },
          (err, stdout, stderr) => {
            if (token.isCancellationRequested) {
              reject(new Cancelled());
              return;
            }
            if (err) {
              /** @type {any} */
              const e = err;
              if (e.code === 'ENOENT') {
                reject(new Error(`Cannot find ${program}. Install webdiff on this computer, or set webdiff.path.`));
                return;
              }
              reject(new Error(String(stderr).trim() || err.message));
              return;
            }
            try {
              resolve(JSON.parse(stdout));
            } catch (e) {
              reject(new Error(`bad output from ${program}: ${e}`));
            }
          },
        );
        token.onCancellationRequested(() => child.kill());
      }),
  );
}

/**
 * askBranch asks for the name of the branch on GitHub.
 *
 * @param {string} prompt
 * @param {string} value
 * @param {(v: string) => string | undefined} [validate]
 * @returns {Promise<string>}
 */
async function askBranch(prompt, value, validate) {
  const branch = await vscode.window.showInputBox({
    title: prompt,
    prompt: 'Name of the branch on GitHub',
    value,
    validateInput: (v) => {
      if (!v.trim()) {
        return 'Type a branch name';
      }
      return validate ? validate(v.trim()) : undefined;
    },
  });
  if (branch === undefined) {
    throw new Cancelled();
  }
  return branch.trim();
}

async function sendToGitHub() {
  const target = await pickTarget();
  const info = await webdiff('webdiff: reading the checkout', ['info', target]);
  if (info.mainRef && info.count === 0) {
    throw new Error(`There are no commits after ${info.main}. Commit the changes first.`);
  }
  const branch = await askBranch(`Send to ${info.repo}`, info.branch, (v) =>
    v === info.main ? `Cannot send to the main branch ${info.main}` : undefined,
  );

  // Show the commits before the push. The push replaces the branch on
  // GitHub, and the working tree is not sent.
  let detail = '';
  if (info.mainRef) {
    const lines = info.commits.map((/** @type {any} */ c) => `${c.sha.slice(0, 10)} ${c.subject}`);
    if (info.count > lines.length) {
      lines.push(`... and ${info.count - lines.length} more`);
    }
    detail = lines.join('\n');
  } else {
    detail = `HEAD is ${info.head.slice(0, 10)}. There is no ${info.main} branch to compare with.`;
  }
  detail += '\n\nThis replaces the branch on GitHub. Changes that are not committed are not sent.';
  const n = info.mainRef ? `${info.count} commit${info.count === 1 ? '' : 's'}` : 'HEAD';
  const ok = await vscode.window.showInformationMessage(
    `Send ${n} to ${info.repo} branch ${branch}?`,
    {
      modal: true,
      detail,
    },
    'Send',
  );
  if (ok !== 'Send') {
    throw new Cancelled();
  }

  // -head makes sure that the push sends the commits that the dialog
  // showed.
  const res = await webdiff(`webdiff: sending ${branch} to GitHub`, [
    'send',
    '-branch',
    branch,
    '-head',
    info.head,
    target,
  ]);
  const open = await vscode.window.showInformationMessage(
    `Sent ${res.head.slice(0, 10)} to ${res.repo} branch ${res.branch}.`,
    'Open on GitHub',
  );
  if (open) {
    vscode.env.openExternal(vscode.Uri.parse(res.compareURL));
  }
}

async function fetchFromGitHub() {
  const target = await pickTarget();
  const info = await webdiff('webdiff: reading the checkout', ['info', target]);
  const branch = await askBranch(`Fetch from ${info.repo}`, info.branch);
  const res = await webdiff(`webdiff: fetching ${branch} from GitHub`, ['fetch', '-branch', branch, target]);
  const msg = fetchMessage(res);
  if (msg.warn) {
    vscode.window.showWarningMessage(msg.text);
  } else {
    vscode.window.showInformationMessage(msg.text);
  }
}

module.exports = {
  activate,
  deactivate,
};
