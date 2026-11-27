// @ts-check
'use strict';

// The parts of the extension that do not use the vscode module, so
// that node can test them.

/**
 * sshTarget returns the "<host>:<path>" argument of "webdiff github"
 * for a workspace folder URI of a Remote-SSH window, or null for other
 * URIs.
 *
 * Remote-SSH puts the host in the authority: "ssh-remote+<host>". For
 * some hosts, for example a host with a user, it uses the hex form of
 * JSON: "ssh-remote+7b22686f73744e616d65223a22...7d".
 *
 * @param {{scheme: string, authority: string, path: string}} uri
 * @returns {string | null}
 */
function sshTarget(uri) {
  if (uri.scheme !== 'vscode-remote') {
    return null;
  }
  const m = /^ssh-remote\+(.+)$/.exec(uri.authority);
  if (!m) {
    return null;
  }
  let host = decodeURIComponent(m[1]);
  if (/^([0-9a-f]{2})+$/i.test(host)) {
    try {
      const j = JSON.parse(Buffer.from(host, 'hex').toString('utf8'));
      if (j && typeof j.hostName === 'string') {
        if (j.port) {
          throw new Error(`Remote-SSH host ${j.hostName} has port ${j.port}; use an ~/.ssh/config alias for it`);
        }
        host = (j.user ? j.user + '@' : '') + j.hostName;
      }
    } catch (e) {
      if (e instanceof SyntaxError) {
        // Not JSON: the host name is only hex digits.
      } else {
        throw e;
      }
    }
  }
  if (!uri.path.startsWith('/')) {
    return null;
  }
  return host + ':' + uri.path;
}

/**
 * shortRef returns the short name of a ref, for example "origin/x"
 * for "refs/remotes/origin/x".
 *
 * @param {string} ref
 */
function shortRef(ref) {
  return ref.replace(/^refs\/(remotes|heads)\//, '');
}

/**
 * fetchMessage returns the message that tells the result of
 * "webdiff github fetch", and if it is a warning.
 *
 * @param {{branch: string, head: string, ref: string, checkout: string, result: string, error?: string}} res
 * @returns {{text: string, warn: boolean}}
 */
function fetchMessage(res) {
  const sha = res.head.slice(0, 10);
  const ref = shortRef(res.ref);
  switch (res.result) {
    case 'fast-forward':
      return {
        text: `Fast-forwarded ${res.branch} to ${sha} from GitHub.`,
        warn: false,
      };
    case 'up-to-date':
      return {
        text: `${res.branch} is up to date with GitHub (${sha}).`,
        warn: false,
      };
    case 'ahead':
      return {
        text: `${res.branch} has all the commits of GitHub (${sha}) and more. It did not change.`,
        warn: false,
      };
    case 'diverged':
      return {
        text: `${res.branch} and GitHub have different commits. The commits of GitHub are at ${ref} (${sha}): merge or rebase them.`,
        warn: true,
      };
    case 'other-branch':
      return {
        text: `Updated ${ref} to ${sha}. The checkout is on ${res.checkout || 'a detached HEAD'}, so it did not change.`,
        warn: false,
      };
    case 'merge-failed':
      return {
        text: `Updated ${ref} to ${sha}, but could not fast-forward ${res.branch}: ${res.error}`,
        warn: true,
      };
    default:
      return {
        text: `Updated ${ref} to ${sha}.`,
        warn: false,
      };
  }
}

module.exports = {
  sshTarget,
  shortRef,
  fetchMessage,
};
