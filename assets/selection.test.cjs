const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {test} = require('node:test');
const vm = require('node:vm');

const app = readFileSync(__dirname + '/app.js', 'utf8');
const start = app.indexOf('  // Store exclusions per checkout.');
const end = app.indexOf('  // Agent picker', start);
const script = '(function(){' + app.slice(start, end) + '})()';

function picker(url, storage = new Map(), values = ['working', 'abc', 'def']) {
  const parsed = new URL(url);
  const explicit = parsed.searchParams.get('changes');
  const choices = values.map(value => ({value, checked: explicit === null || explicit.split(',').includes(value)}));
  const elements = new Map();
  function element(id) {
    if (!elements.has(id)) elements.set(id, {
      hidden: true,
      addEventListener(event, fn) { this[event] = fn; },
      querySelectorAll() { return choices; },
      setAttribute() {},
      contains() { return false; },
    });
    return elements.get(id);
  }
  const location = {href: url, pathname: parsed.pathname, replace(url) { this.replaced = url; }};
  vm.runInNewContext(script, {
    URL, location,
    localStorage: {
      getItem(key) { return storage.get(key) ?? null; },
      setItem(key, value) { storage.set(key, value); },
      removeItem(key) { storage.delete(key); },
    },
    document: {getElementById: element, body: {appendChild() {}}, addEventListener() {}},
    window: {addEventListener() {}},
  });
  return {location, choices, click(id) { element(id).click(); }};
}

const url = 'http://localhost/worktrees/repo-feature/';
test('all changes default; legacy mode is removed', () => {
  const view = picker(url);
  assert.ok(view.choices.every(c => c.checked));
  assert.equal(view.location.replaced, undefined);
  assert.equal(picker(url + '?mode=branch&context=full').location.replaced, url + '?context=full');
});

test('remember exclusions per checkout and select new commits', () => {
  const storage = new Map();
  const view = picker(url, storage);
  view.choices[1].checked = false;
  view.click('diff-picker-apply');
  assert.equal(new URL(view.location.href).searchParams.get('changes'), 'working,def');
  const restored = picker(url, storage, ['working', 'def', 'new']);
  assert.equal(restored.location.replaced, undefined);
  assert.equal(new URL(picker(url, storage).location.replaced).searchParams.get('changes'), 'working,def');
  assert.equal(picker('http://localhost/worktrees/repo-other/', storage).location.replaced, undefined);
});

test('empty selection persists; all clears saved exclusions', () => {
  const storage = new Map();
  const view = picker(url, storage);
  view.click('diff-picker-none');
  view.click('diff-picker-apply');
  assert.equal(view.location.href, url + '?changes=');
  assert.equal(picker(url, storage).location.replaced, url + '?changes=');
  const empty = picker(view.location.href, storage);
  empty.click('diff-picker-all');
  empty.click('diff-picker-apply');
  assert.equal(empty.location.href, url);
  assert.equal(storage.size, 0);
});

test('explicit URL overrides saved choices', () => {
  const storage = new Map([['webdiff:changes:/worktrees/repo-feature/', '["abc"]']]);
  picker(url + '?changes=abc', storage);
  assert.equal(new URL(picker(url, storage).location.replaced).searchParams.get('changes'), 'abc');
});

test('invalid or unavailable storage does not prevent navigation', () => {
  const storage = new Map([['webdiff:changes:/worktrees/repo-feature/', '{bad json']]);
  assert.equal(picker(url, storage).location.replaced, undefined);
  storage.get = () => { throw new Error('unavailable'); };
  storage.set = storage.delete = storage.get;
  const view = picker(url, storage);
  view.click('diff-picker-none');
  view.click('diff-picker-apply');
  assert.equal(view.location.href, url + '?changes=');
});
