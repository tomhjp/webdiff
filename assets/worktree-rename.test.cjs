const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {test} = require('node:test');
const vm = require('node:vm');

const app = readFileSync(__dirname + '/app.js', 'utf8');
const start = app.indexOf('  function setupWorktreeRename(){');
const end = app.indexOf('  function setupWorktreeRemove(){', start);
const script = app.slice(start, end) + '\nsetupWorktreeRename();';

function view({stream = false, branch = 'tomhjp/new', ok = true, button = true} = {}) {
  const btn = {
    dataset: {name: 'repo-old', branch: 'tomhjp/old'},
    addEventListener(event, fn) { this[event] = fn; },
  };
  const requests = [];
  const alerts = [];
  const location = {href: 'http://localhost/worktrees/repo-old/?context=full', search: '?context=full'};
  vm.runInNewContext(script, {
    URL, location, streamRoot: stream ? {} : null,
    document: {getElementById() { return button ? btn : null; }},
    window: {prompt() { return branch; }, alert(message) { alerts.push(message); }},
    fetch(url, options) {
      requests.push({url, options});
      return Promise.resolve({ok, json: () => Promise.resolve({url: '/worktrees/repo-new/'}), text: () => Promise.resolve('branch already exists')});
    },
  });
  return {btn, requests, alerts, location};
}

async function click(page) {
  page.btn.click();
  await new Promise(resolve => setImmediate(resolve));
}

test('diff rename sends the branch and preserves diff query', async () => {
  const page = view({branch: ' tomhjp/new '});
  await click(page);
  assert.equal(page.requests[0].url, '/api/worktrees/rename');
  assert.deepEqual(JSON.parse(page.requests[0].options.body), {name: 'repo-old', branch: 'tomhjp/new'});
  assert.equal(page.location.href, 'http://localhost/worktrees/repo-new/?context=full');
});

test('pane rename returns to the renamed stream', async () => {
  const page = view({stream: true});
  await click(page);
  assert.equal(page.location.href, 'http://localhost/stream/worktrees/repo-new/');
});

test('cancel and empty names do not send requests', async () => {
  for (const branch of [null, '  ']) {
    const page = view({branch});
    await click(page);
    assert.equal(page.requests.length, 0);
  }
});

test('failure restores the button and displays the error', async () => {
  const page = view({ok: false});
  await click(page);
  assert.equal(page.btn.disabled, false);
  assert.equal(page.btn.textContent, 'rename');
  assert.match(page.alerts[0], /branch already exists/);
  assert.match(page.location.href, /repo-old/);
});

test('pages without the rename button have no rename handler', () => {
  const page = view({button: false});
  assert.equal(page.btn.click, undefined);
  assert.equal(page.requests.length, 0);
});
