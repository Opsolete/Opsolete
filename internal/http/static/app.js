// Opsolete's UI behaviour. The CSP allows no inline script: pages mark elements with data-*
// attributes and the listeners below act on them. Loaded with defer: the page is parsed when it runs.
"use strict";

// Opsolete's own confirm(): it stops the submit event e, asks msg in #confirm-dialog and submits the
// form again (with the same button) once confirmed. word: the user must type it, like when deleting a workspace.
function opsConfirm(e, msg, word) {
  const f = e.target, by = e.submitter;
  if (f.dataset.confirmed) { delete f.dataset.confirmed; return true; }
  e.preventDefault();
  const d = document.getElementById('confirm-dialog'), $ = id => document.getElementById(id);
  const label = (by?.textContent || 'Confirm').trim().replace(/…$/, ''), danger = word || by?.classList.contains('danger');
  d.className = danger ? 'danger-dialog' : '';
  $('confirm-title').textContent = (danger ? '⚠ ' : '') + label;
  $('confirm-msg').textContent = msg;
  $('confirm-typed').hidden = !word;
  Object.assign($('confirm-input'), {value: '', required: !!word, pattern: word || '.*'});
  $('confirm-typed').querySelector('label').textContent = `To confirm, type "${word}"`;
  Object.assign($('confirm-ok'), {textContent: label, className: danger ? 'danger' : ''});
  d.querySelector('form').onsubmit = () => { f.dataset.confirmed = 1; f.requestSubmit(by); };
  d.showModal();
  return false;
}

// <form data-confirm="message">: asks first. data-confirm-word: the word to type;
// data-confirm-when="name": only while the form's checkbox name is checked and enabled.
document.addEventListener('submit', e => {
  const f = e.target, when = f.dataset.confirmWhen;
  if (f.dataset.confirm === undefined) return;
  if (when && (!f.elements[when].checked || f.elements[when].disabled)) return;
  opsConfirm(e, f.dataset.confirm, f.dataset.confirmWord);
});

// filters a picker (layout.html "picker") by the search text and "Show selected", and shows the count in its summary
function filterPicker(el) {
  const d = el.closest('details'), q = d.querySelector('input[type=search]').value.toLowerCase();
  const only = d.querySelector('[aria-pressed]').ariaPressed === 'true';
  let n = 0;
  for (const l of d.querySelectorAll('.picker-list label')) {
    const box = l.querySelector('input');
    l.hidden = !l.textContent.toLowerCase().includes(q) || (only && !box.checked);
    n += box.checked;
  }
  d.querySelector('summary span').textContent = n ? n + ' selected' : 'Select workspaces';
}

document.addEventListener('click', e => {
  const t = e.target.closest('[data-show-dialog], [data-close-dialog], [data-copy], [data-log], .picker [aria-pressed]');
  const log = document.getElementById('log'); // run.html's
  if (t?.dataset.showDialog) document.getElementById(t.dataset.showDialog).showModal();
  else if (t?.hasAttribute('data-close-dialog')) t.closest('dialog').close();
  else if (t?.dataset.copy) navigator.clipboard.writeText(t.dataset.copy);
  else if (t?.dataset.log === 'top') log.scrollTop = 0;
  else if (t?.dataset.log === 'bottom') log.scrollTop = log.scrollHeight;
  else if (t?.dataset.log === 'fullscreen') log.requestFullscreen();
  else if (t) { t.setAttribute('aria-pressed', t.ariaPressed !== 'true'); filterPicker(t); } // a picker's "Show selected"
  // a click outside closes a picker or a row's actions menu, like a select
  for (const d of document.querySelectorAll('details.picker[open], details.kebab[open]')) if (!d.contains(e.target)) d.open = false;
});

document.addEventListener('input', e => {
  const t = e.target;
  if (t.matches('.picker input[type=search]')) filterPicker(t);
  else if (t.hasAttribute('data-filter-resources')) { // the workspace's resources: rows, then groups left empty
    const q = t.value.toLowerCase();
    document.querySelectorAll('#resources tr[data-row]').forEach(tr => tr.hidden = !tr.textContent.toLowerCase().includes(q));
    document.querySelectorAll('#resources details').forEach(d => d.hidden = !d.querySelector('tr[data-row]:not([hidden])'));
  } else if (t.hasAttribute('data-filter-projects')) { // shows the projects whose name contains the search text
    const q = t.value.trim().toLowerCase();
    let n = 0;
    for (const tr of document.querySelectorAll('tr[data-name]')) {
      tr.hidden = !tr.dataset.name.includes(q);
      n += !tr.hidden;
    }
    document.getElementById('no-projects').hidden = n > 0;
  }
});

document.addEventListener('keydown', e => {
  if (e.key === 'Enter' && e.target.matches('.picker input[type=search]')) e.preventDefault(); // else it submits the form
});

document.addEventListener('change', e => {
  const t = e.target, choice = t.closest('[data-menu-choice]');
  if (t.closest('.picker-list')) filterPicker(t);
  else if (t.dataset.navigate !== undefined) location = t.dataset.navigate + encodeURIComponent(t.value);
  else if (t.closest('[data-refresh]')) { // the server shows the form again for the new choice, typed values kept
    t.form.append(Object.assign(document.createElement('input'), {type: 'hidden', name: 'refresh', value: '1'}));
    t.form.submit(); // not requestSubmit: no validation, no "Starting plan…"
  } else if (choice) { // radios in a <details> menu: the summary mirrors the choice
    const d = choice.closest('details');
    d.querySelector('summary span').textContent = t.closest('label').querySelector('strong').textContent;
    d.open = false;
  }
});
addEventListener('pageshow', () => { for (const el of document.querySelectorAll('input[name=refresh]')) el.remove(); }); // Back restores the page as left

// <dialog data-open>: re-shown with a refused form, it opens modally, at its first invalid field
for (const d of document.querySelectorAll('dialog[data-open]')) {
  d.showModal();
  d.querySelector('[aria-invalid="true"]')?.focus();
}

// run.html: follow the log live; reload once a runner is done with the run (finished, or waiting for confirmation).
(() => {
  const st = document.querySelector('[data-run-status]');
  if (!st) return;
  const log = document.getElementById('log'), wasWorking = st.dataset.runWorking === 'true';
  const es = new EventSource(location.pathname + '/logs/stream');
  // Terraform's own colors: the log carries its ANSI color codes (SGR), rendered as ansi-* classes.
  const attrs = {1: 'b', 2: 'dim', 4: 'u'}, style = new Set();
  let fg = '', first = true, rest = '';
  const render = text => text.split(/\x1b\[([\d;]*)m/).forEach((t, i) => {
    if (i % 2) { // a code list, e.g. "0" or "1;32"; "" is a reset
      for (const c of t.split(';').map(Number)) {
        if (c === 0) { style.clear(); fg = ''; }
        else if (attrs[c]) style.add(attrs[c]);
        else if (c === 22) { style.delete('b'); style.delete('dim'); }
        else if (c === 24) style.delete('u');
        else if (c === 39) fg = '';
        else if ((c >= 30 && c <= 37) || (c >= 90 && c <= 97)) fg = 'c' + c;
      }
    } else if (t) {
      const cls = [...style, fg].filter(Boolean).map(c => 'ansi-' + c).join(' ');
      log.append(cls ? Object.assign(document.createElement('span'), {className: cls, textContent: t}) : t);
    }
  });
  es.onmessage = e => {
    if (!log) return;
    if (first) { log.textContent = ''; first = false; }
    const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
    const text = rest + e.data, cut = text.search(/\x1b(\[[\d;]*)?$/); // a code split over two chunks
    rest = cut < 0 ? '' : text.slice(cut);
    render(cut < 0 ? text : text.slice(0, cut)); // nodes: += would re-render the whole log
    if (atBottom) log.scrollTop = log.scrollHeight;
  };
  es.addEventListener('status', e => { if (e.data !== st.dataset.runStatus) location.reload(); });
  es.addEventListener('done', () => { es.close(); if (wasWorking) location.reload(); });
})();

// form.html: show_if hides and disables the fields whose dropdown above doesn't have one of their values
// (disabled ones aren't posted); the server applies the same rules. Document order puts each dropdown first.
(() => {
  const form = document.getElementById('template-form');
  if (!form) return;
  const update = () => {
    for (const el of form.querySelectorAll('[data-show-if]')) {
      const c = JSON.parse(el.dataset.showIf), src = form.elements[c.input];
      const v = src?.type === 'checkbox' ? String(src.checked) : src?.value;
      // a project's value (data-locked) is disabled so it isn't posted; it counts unless its row is hidden
      const off = !src || ('locked' in src.dataset ? src.closest('fieldset').disabled : src.matches(':disabled'));
      el.hidden = el.disabled = off || !c.equals.includes(v);
    }
  };
  form.addEventListener('change', update);
  update(); // the browser may have restored other values
  // Browsers compile pattern with the v flag, which rejects e.g. [a-z0-9-] and then skip the check:
  // check those with the u flag instead. Go-only syntax fails both; the server still checks it.
  for (const el of form.querySelectorAll('input[pattern]')) {
    const p = el.getAttribute('pattern');
    try { new RegExp(p, 'v'); continue; } catch {}
    let re;
    try { re = new RegExp('^(?:' + p + ')$', 'u'); } catch { continue; }
    const check = () => el.setCustomValidity(el.value && !re.test(el.value) ? el.title || "Doesn't have the expected format: " + p : '');
    el.addEventListener('input', check);
    check();
  }
  // one click, one plan: lock the button while the request runs; unlock when Back restores the page
  const btn = form.querySelector('button[type=submit]'), text = btn?.textContent; // none: the user may create no workspace
  form.addEventListener('submit', () => { btn.disabled = true; btn.textContent = 'Starting plan…'; });
  addEventListener('pageshow', () => { if (btn) { btn.disabled = false; btn.textContent = text; } });
})();

// builder.html, progressive enhancement: without JS the last card is the blank one to fill in, and you
// clear a variable name to remove a field. The server checks everything again on save.
(() => {
  const list = document.getElementById('fields'), add = document.getElementById('add-field');
  if (!list || !add) return;
  const cards = () => [...list.querySelectorAll('.fcard')];
  const split = s => s.split(',').map(v => v.trim()).filter(Boolean);
  const optionValues = card => card.querySelector('.ftype').value === 'bool' ? ['true', 'false'] :
    [...card.querySelectorAll(".opt-row [aria-label='Option value']")].map(el => el.value.trim()).filter(Boolean);
  // Renumber the checkboxes and option rows (they post their card's index), offer each card the
  // dropdowns above it, and show the chosen dropdown's values as chips to click.
  // "Module name from" follows its card through renames; a card that can't be the key drops it.
  const key = document.getElementById('key'), nameOf = card => card.querySelector('[name=in_name]').value.trim();
  let keyCard = cards().find(c => key.value && nameOf(c) === key.value);
  key.onchange = () => keyCard = cards().find(c => nameOf(c) === key.value);
  const refresh = () => {
    const keys = cards().filter(c => {
      const t = c.querySelector('.ftype').value;
      return nameOf(c) && t !== 'bool' && !(t !== 'select' && c.querySelector('[name=in_sensitive]').checked) &&
        !c.querySelector('[name=in_per_environment]').checked;
    });
    if (!keys.includes(keyCard)) keyCard = undefined;
    key.replaceChildren(new Option("the template's name", ''), ...keys.map(c => new Option(nameOf(c), nameOf(c), c === keyCard, c === keyCard)));
    const above = new Map();
    cards().forEach((card, i) => {
      for (const box of card.querySelectorAll('[type=checkbox]')) box.value = i;
      for (const el of card.querySelectorAll('.opt-row input')) el.name = el.name.replace(/\d*$/, i);
      const sel = card.querySelector('.show-input'), cur = sel.value, names = [...above.keys()];
      if (cur && !names.includes(cur)) names.push(cur); // keep a stale choice: the save explains it
      if (sel.options.length !== names.length + 1 || names.some((n, j) => sel.options[j + 1].value !== n))
        sel.replaceChildren(new Option('always', ''), ...names.map(n => new Option('when ' + n + ' is', n, n === cur, n === cur)));
      const eq = card.querySelector('.show-equals'), picked = split(eq.value);
      const chips = card.querySelector('.show-chips'), chipVals = above.has(cur) ? optionValues(above.get(cur)) : [];
      // rebuilt only when the values change: the change event of the field just left (fired on the
      // chip's mousedown) must not replace the chip before its click lands
      if (chips.children.length !== chipVals.length || chipVals.some((v, j) => chips.children[j].dataset.v !== v))
        chips.replaceChildren(...chipVals.map(v => {
          const b = document.createElement('button');
          b.type = 'button'; b.className = 'chip'; b.textContent = b.dataset.v = v;
          return b;
        }));
      for (const b of chips.children) b.setAttribute('aria-pressed', picked.includes(b.dataset.v));
      // a number's default is typed as a number; a dropdown's suggests its option values (Yes / no has its own select)
      const type = card.querySelector('.ftype').value, def = card.querySelector('[name=in_default]'), dl = def.nextElementSibling;
      if (def.type !== (type === 'number' ? 'number' : 'text')) Object.assign(def, type === 'number' ? {type: 'number', step: 'any'} : {type: 'text'});
      dl.id = 'defaults-' + i;
      def.setAttribute('list', dl.id);
      def.placeholder = type === 'select' ? 'the first option' : 'none';
      const vals = type === 'select' ? optionValues(card) : [];
      if (dl.options.length !== vals.length || vals.some((v, j) => dl.options[j].value !== v)) dl.replaceChildren(...vals.map(v => new Option(v, v)));
      const name = card.querySelector('[name=in_name]').value.trim();
      if (name && (type === 'select' || type === 'bool')) above.set(name, card);
    });
  };
  list.addEventListener('click', e => {
    const chip = e.target.closest('.chip'), act = e.target.closest('[data-act]');
    if (chip) {
      const eq = chip.parentElement.parentElement.querySelector('.show-equals'), vs = split(eq.value), v = chip.dataset.v;
      eq.value = (vs.includes(v) ? vs.filter(x => x !== v) : [...vs, v]).join(', ');
    } else if (act?.dataset.act === 'opt-add') {
      const rows = act.previousElementSibling, row = rows.lastElementChild.cloneNode(true);
      for (const el of row.querySelectorAll('input')) el.value = '';
      rows.append(row);
      row.querySelector('input').focus();
    } else if (act?.dataset.act === 'opt-remove') {
      const row = act.closest('.opt-row'), rows = row.parentElement;
      if (rows.children.length > 1) row.remove(); else for (const el of row.querySelectorAll('input')) el.value = '';
      rows.querySelector('input').focus();
    } else if (act) {
      const card = act.closest('.fcard');
      if (act.dataset.act === 'up') card.previousElementSibling?.before(card);
      if (act.dataset.act === 'down') card.nextElementSibling?.after(card);
      if (act.dataset.act === 'remove') {
        if (cards().length > 1) card.remove(); else reset(card);
      } else act.focus();
    } else return;
    refresh();
  });
  const reset = card => {
    for (const row of [...card.querySelectorAll('.opt-row')].slice(1)) row.remove();
    for (const el of card.querySelectorAll('input')) el.type === 'checkbox' ? el.checked = false : el.value = '';
    for (const el of card.querySelectorAll('select')) el.selectedIndex = 0;
  };
  add.onclick = () => {
    const card = list.lastElementChild.cloneNode(true);
    reset(card);
    list.append(card);
    refresh();
    card.querySelector('[name=in_name]').focus();
  };
  // picking a registry module reloads the page: carry the typed name and description along, and
  // confirm before its variables replace the fields
  const pick = document.getElementById('from-module'), builder = document.getElementById('builder');
  pick?.addEventListener('submit', e => {
    if (cards().some(nameOf) && !opsConfirm(e, "Replace the form fields below with the module's variables? Unsaved field changes are lost.")) return e.preventDefault();
    for (const n of ['name', 'description'])
      (pick.elements[n] ?? pick.appendChild(Object.assign(document.createElement('input'), {type: 'hidden', name: n}))).value = builder.elements[n].value;
  });
  list.addEventListener('input', refresh);
  list.addEventListener('change', refresh);
  for (const el of [add, ...document.querySelectorAll('.fcard-tools, [data-act^=opt]')]) el.hidden = false;
  // "+ Add field" replaces the blank card the page ends with for use without JS
  if (cards().length > 1 && !nameOf(list.lastElementChild)) list.lastElementChild.remove();
  refresh();
})();
