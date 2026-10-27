const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {test} = require('node:test');
const vm = require('node:vm');

const app = readFileSync(__dirname + '/app.js', 'utf8');
const script = app.slice(app.indexOf('  function setupDiffMore(){'), app.indexOf('  // "full context" toggle:'));

function toolbar(present = true) {
  const events = {};
  const windowEvents = {};
  const first = {focus() { this.focused = true; }};
  const toggle = {
    events: {}, attrs: {},
    addEventListener(type, fn) { this.events[type] = fn; },
    setAttribute(key, value) { this.attrs[key] = value; },
    getBoundingClientRect() { return {bottom: 90}; },
    contains(target) { return target === this; },
    focus() { this.focused = true; },
  };
  const menu = {
    hidden: true, style: {}, events: {},
    addEventListener(type, fn) { this.events[type] = fn; },
    contains(target) { return target === this || target === first; },
    querySelector() { return first; },
  };
  const document = {
    getElementById(id) { return present ? (id === 'diff-more-toggle' ? toggle : menu) : null; },
    body: {appendChild(child) { child.appended = true; }},
    addEventListener(type, fn) { events[type] = fn; },
  };
  vm.runInNewContext(script, {
    document,
    window: {innerHeight: 600, addEventListener(type, fn) { windowEvents[type] = fn; }},
  });
  return {toggle, menu, first, events, windowEvents};
}

test('more starts hidden and opens a positioned menu with keyboard focus', () => {
  const {toggle, menu, first} = toolbar();
  assert.equal(menu.hidden, true);
  assert.equal(menu.appended, true);
  toggle.events.click();
  assert.equal(menu.hidden, false);
  assert.equal(toggle.attrs['aria-expanded'], 'true');
  assert.equal(menu.style.top, '94px');
  assert.equal(first.focused, true);
  toggle.events.click();
  assert.equal(menu.hidden, true);
  assert.equal(toggle.attrs['aria-expanded'], 'false');
});

test('Escape dismisses the menu and returns focus to more', () => {
  const {toggle, menu, events} = toolbar();
  toggle.events.click();
  let prevented = false;
  events.keydown({key: 'Escape', preventDefault() { prevented = true; }});
  assert.equal(menu.hidden, true);
  assert.equal(toggle.focused, true);
  assert.equal(prevented, true);
});

test('outside interaction, resize, and page scroll dismiss the menu', () => {
  const {toggle, menu, first, events, windowEvents} = toolbar();
  for (const event of ['click', 'focusin']) {
    toggle.events.click();
    events[event]({target: first});
    assert.equal(menu.hidden, false);
    events[event]({target: {}});
    assert.equal(menu.hidden, true);
  }
  for (const event of ['resize', 'scroll']) {
    toggle.events.click();
    windowEvents[event]();
    assert.equal(menu.hidden, true);
  }
});

test('choosing an option closes the menu', () => {
  const {toggle, menu} = toolbar();
  toggle.events.click();
  menu.events.click({target: {closest() { return {}; }}});
  assert.equal(menu.hidden, true);
});

test('other pages need no options menu', () => {
  assert.doesNotThrow(() => toolbar(false));
});
