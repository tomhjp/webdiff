const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {test} = require('node:test');
const vm = require('node:vm');

const app = readFileSync(__dirname + '/app.js', 'utf8');
const start = app.indexOf('  function setupBranchIssues(){');
const end = app.indexOf('\n  }\n', start) + 4;
const script = app.slice(start, end) + '\nsetupBranchIssues();';

function element(props = {}) {
  return Object.assign({
    listeners: {},
    dataset: {},
    parentNode: {},
    addEventListener(event, fn) { this.listeners[event] = fn; },
    setAttribute(k, v) { this[k] = v; },
    replaceWith(other) { this.replacedWith = other; this.parentNode = null; other.parentNode = {}; },
    focus() {},
    setSelectionRange() {},
  }, props);
}

function view({manual = '', repo = 'tailscale/corp', ok = true} = {}) {
  const box = element({dataset: {branch: 'tomhjp/foo', repo, manual}});
  const btn = element({closest() { return box; }});
  const requests = [];
  const alerts = [];
  let reloaded = false;
  vm.runInNewContext(script, {
    setTimeout: fn => fn(),
    location: {reload() { reloaded = true; }},
    document: {
      querySelectorAll(sel) { return sel === '.branch-issues .issue-edit' ? [btn] : []; },
      createElement() { return element(); },
    },
    window: {alert(message) { alerts.push(message); }},
    postJSON(url, body) {
      requests.push({url, body});
      return ok ? Promise.resolve({}) : Promise.reject(new Error('bad issue'));
    },
  });
  return {btn, box, requests, alerts, reloaded: () => reloaded};
}

async function settle() {
  await new Promise(resolve => setImmediate(resolve));
}

test('the edit button changes into a field with the issues that the user wrote', () => {
  const page = view({manual: '#1 oss#2'});
  page.btn.listeners.click();
  const input = page.btn.replacedWith;
  assert.equal(input.className, 'issue-input');
  assert.equal(input.value, '#1 oss#2');
});

test('enter saves the issues of the branch and loads the page again', async () => {
  const page = view();
  page.btn.listeners.click();
  const input = page.btn.replacedWith;
  input.value = '#12, corp#3';
  input.listeners.keydown({key: 'Enter', preventDefault() {}});
  await settle();
  assert.deepEqual(JSON.parse(JSON.stringify(page.requests)), [{url: '/api/branch-issues', body: {branch: 'tomhjp/foo', repo: 'tailscale/corp', issues: '#12, corp#3'}}]);
  assert.equal(page.reloaded(), true);
});

test('escape puts the button back', () => {
  const page = view();
  page.btn.listeners.click();
  const input = page.btn.replacedWith;
  input.listeners.keydown({key: 'Escape', preventDefault() {}});
  assert.equal(input.replacedWith, page.btn);
});

test('an error keeps the field and shows the message', async () => {
  const page = view({ok: false});
  page.btn.listeners.click();
  const input = page.btn.replacedWith;
  input.value = 'nope';
  input.listeners.keydown({key: 'Enter', preventDefault() {}});
  await settle();
  assert.equal(page.reloaded(), false);
  assert.match(page.alerts[0], /bad issue/);
  assert.equal(input.replacedWith, undefined);
  assert.equal(input.disabled, false);
});
