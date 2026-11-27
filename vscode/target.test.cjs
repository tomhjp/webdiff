'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { sshTarget, shortRef, fetchMessage } = require('./target.js');

test('sshTarget', () => {
  const hex = (o) => Buffer.from(JSON.stringify(o)).toString('hex');
  const cases = [
    [{ scheme: 'vscode-remote', authority: 'ssh-remote+slop', path: '/home/ubuntu/ai/wt/x/y' }, 'slop:/home/ubuntu/ai/wt/x/y'],
    [{ scheme: 'vscode-remote', authority: 'ssh-remote+ubuntu%40slop', path: '/a' }, 'ubuntu@slop:/a'],
    [{ scheme: 'vscode-remote', authority: 'ssh-remote+' + hex({ hostName: 'slop', user: 'ubuntu' }), path: '/a' }, 'ubuntu@slop:/a'],
    // A host name that is only hex digits.
    [{ scheme: 'vscode-remote', authority: 'ssh-remote+beef', path: '/a' }, 'beef:/a'],
    [{ scheme: 'vscode-remote', authority: 'dev-container+abc', path: '/a' }, null],
    [{ scheme: 'file', authority: '', path: '/a' }, null],
  ];
  for (const [uri, want] of cases) {
    assert.strictEqual(sshTarget(uri), want, JSON.stringify(uri));
  }
  assert.throws(() =>
    sshTarget({ scheme: 'vscode-remote', authority: 'ssh-remote+' + hex({ hostName: 'slop', port: 2222 }), path: '/a' }),
  );
});

test('shortRef', () => {
  assert.strictEqual(shortRef('refs/remotes/origin/tomhjp/x'), 'origin/tomhjp/x');
});

test('fetchMessage', () => {
  const res = {
    branch: 'tomhjp/x',
    head: '0123456789abcdef',
    ref: 'refs/remotes/origin/tomhjp/x',
    checkout: 'tomhjp/x',
  };
  assert.deepStrictEqual(fetchMessage({ ...res, result: 'fast-forward' }), {
    text: 'Fast-forwarded tomhjp/x to 0123456789 from GitHub.',
    warn: false,
  });
  assert.strictEqual(fetchMessage({ ...res, result: 'diverged' }).warn, true);
  assert.match(fetchMessage({ ...res, result: 'other-branch', checkout: '' }).text, /detached HEAD/);
  assert.match(fetchMessage({ ...res, result: 'merge-failed', error: 'boom' }).text, /boom$/);
});
