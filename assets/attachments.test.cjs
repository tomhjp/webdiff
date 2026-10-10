const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {test} = require('node:test');
const vm = require('node:vm');

const app = readFileSync(__dirname + '/app.js', 'utf8');
const start = app.indexOf('    function openAttachModal(){');
const end = app.indexOf('    if(attachBtn)', start);

function modal(failed = []) {
  const elements = [];
  const uploads = [];
  const alerts = [];
  function createElement(tag) {
    const listeners = new Map();
    const el = {
      tag, files: [], value: '', children: [],
      appendChild(child) { this.children.push(child); },
      addEventListener(event, fn) {
        if (!listeners.has(event)) listeners.set(event, []);
        listeners.get(event).push(fn);
      },
      emit(event, data = {}) {
        for (const fn of listeners.get(event) || []) fn(data);
      },
      click() { this.emit('click'); },
      remove() { this.removed = true; },
      focus() {},
      classList: {add() {}, remove() {}},
    };
    elements.push(el);
    return el;
  }
  vm.runInNewContext(app.slice(start, end) + '\nopenAttachModal();', {
    document: {createElement, body: {appendChild() {}}},
    window: {alert(message) { alerts.push(message); }},
    setTimeout() {}, TextEncoder,
    pickFileName() { return 'paste.txt'; },
    readFileBytes(file) { return Promise.resolve(file.name); },
    uploadAttachment(name, bytes) {
      uploads.push({name, bytes});
      return failed.includes(name) ? Promise.reject(new Error('upload failed')) : Promise.resolve();
    },
  });
  return {
    uploads, alerts,
    find(cls) { return elements.find(el => el.className === cls); },
    async settle() { await new Promise(resolve => setImmediate(resolve)); },
  };
}

test('file picker accepts and uploads multiple files before closing', async () => {
  const view = modal();
  const picker = view.find('attach-file-input');
  assert.equal(picker.multiple, true);
  picker.files = [{name: 'one.txt'}, {name: 'two.txt'}];
  picker.emit('change');
  assert.equal(view.find('modal-backdrop').removed, undefined);
  assert.equal(view.find('toggle send-btn').disabled, true);
  await view.settle();
  assert.deepEqual(view.uploads.map(u => u.name), ['one.txt', 'two.txt']);
  assert.equal(view.find('modal-backdrop').removed, true);
});

test('drop uploads every file and ignores another drop while busy', async () => {
  const view = modal();
  const drop = view.find('attach-drop');
  const event = files => ({preventDefault() {}, dataTransfer: {files}});
  drop.emit('drop', event([{name: 'one.txt'}, {name: 'two.txt'}]));
  drop.emit('drop', event([{name: 'extra.txt'}]));
  await view.settle();
  assert.deepEqual(view.uploads.map(u => u.name), ['one.txt', 'two.txt']);
});

test('failed uploads do not prevent other files from uploading', async () => {
  const view = modal(['one.txt']);
  const picker = view.find('attach-file-input');
  picker.files = [{name: 'one.txt'}, {name: 'two.txt'}];
  picker.emit('change');
  await view.settle();
  assert.deepEqual(view.uploads.map(u => u.name), ['one.txt', 'two.txt']);
  assert.equal(view.find('modal-backdrop').removed, undefined);
  assert.equal(view.find('toggle send-btn').disabled, false);
  assert.match(view.alerts[0], /one\.txt: upload failed/);
});

test('canceling selection does nothing and pasted text still uploads', async () => {
  const view = modal();
  view.find('attach-file-input').emit('change');
  await view.settle();
  assert.equal(view.uploads.length, 0);
  view.find('attach-text').value = 'context';
  view.find('toggle send-btn').click();
  await view.settle();
  assert.equal(view.uploads[0].name, 'paste.txt');
  assert.equal(new TextDecoder().decode(view.uploads[0].bytes), 'context');
  assert.equal(view.find('modal-backdrop').removed, true);
});
