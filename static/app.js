// hop landing UI — vanilla JS, no dependencies. Talks only to this origin's /api/*.
// The token lives in localStorage ("hop.token") and is sent as a Bearer header.
(function () {
  'use strict';
  var body = document.body;
  var kind = body.getAttribute('data-kind') === 'pastes' ? 'pastes' : 'links';
  var KEY = 'hop.token';
  var $ = function (id) { return document.getElementById(id); };
  var tokenInput = $('token'), unlockBtn = $('unlock'), forgetBtn = $('forget'), tokenStatus = $('token-status');
  var form = $('create-form'), createErr = $('create-error');
  var resultCard = $('result-card'), resultURL = $('result-url'), resultMeta = $('result-meta'), copyResult = $('copy-result');
  var listCard = $('list-card'), listBody = document.querySelector('#list tbody'), listEmpty = $('list-empty'), refreshBtn = $('refresh');
  if (!tokenInput || !unlockBtn || !form) return;

  var token = '';
  try { token = localStorage.getItem(KEY) || ''; } catch (e) { token = ''; }

  function show(el, on) { if (el) el.classList.toggle('hidden', !on); }
  function setStatus(text, cls) {
    tokenStatus.textContent = text;
    tokenStatus.className = 'muted small' + (cls ? ' ' + cls : '');
  }
  function saveToken(t) {
    token = t;
    try { if (t) localStorage.setItem(KEY, t); else localStorage.removeItem(KEY); } catch (e) { /* private mode: keep in memory */ }
  }
  function headers(extra) {
    var h = { 'Authorization': 'Bearer ' + token };
    if (extra) for (var k in extra) if (extra[k] !== undefined && extra[k] !== '') h[k] = extra[k];
    return h;
  }
  function api(method, path, opts) {
    opts = opts || {};
    return fetch(path, { method: method, headers: headers(opts.headers), body: opts.body, cache: 'no-store' })
      .then(function (r) {
        if (r.status === 204) return { ok: true, status: 204, data: null };
        return r.text().then(function (t) {
          var data = null; try { data = t ? JSON.parse(t) : null; } catch (e) { data = { error: t }; }
          return { ok: r.ok, status: r.status, data: data };
        });
      });
  }
  function errText(res) {
    if (res.status === 401) return 'token rejected — check it and unlock again';
    if (res.status === 503) return 'writes are disabled on this instance';
    if (res.status === 429) return 'slow down — rate limited';
    return (res.data && res.data.error) ? res.data.error : ('request failed (HTTP ' + res.status + ')');
  }
  function fmtDate(s) { if (!s) return '—'; var d = new Date(s); return isNaN(d) ? s : d.toISOString().slice(0, 16).replace('T', ' ') + ' UTC'; }
  function fmtSize(n) { return n >= 1048576 ? (n / 1048576).toFixed(1) + ' MiB' : n >= 1024 ? (n / 1024).toFixed(1) + ' KiB' : n + ' B'; }
  function trunc(s, n) { return s && s.length > n ? s.slice(0, n - 1) + '…' : s; }
  function copy(text, btn) {
    var done = function () { if (btn) { var old = btn.textContent; btn.textContent = 'Copied'; setTimeout(function () { btn.textContent = old; }, 1200); } };
    if (navigator.clipboard && navigator.clipboard.writeText) { navigator.clipboard.writeText(text).then(done, function () { fallbackCopy(text); done(); }); }
    else { fallbackCopy(text); done(); }
  }
  function fallbackCopy(text) {
    var ta = document.createElement('textarea'); ta.value = text; ta.setAttribute('readonly', '');
    ta.style.position = 'fixed'; ta.style.opacity = '0'; document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); } catch (e) { /* ignore */ } document.body.removeChild(ta);
  }
  function el(tag, attrs, children) {
    var e = document.createElement(tag);
    if (attrs) for (var k in attrs) { if (k === 'text') e.textContent = attrs[k]; else if (k === 'class') e.className = attrs[k]; else e.setAttribute(k, attrs[k]); }
    (children || []).forEach(function (c) { e.appendChild(c); });
    return e;
  }

  // --- lock / unlock -----------------------------------------------------------
  function lock() {
    saveToken('');
    tokenInput.value = '';
    show(form, false); show(resultCard, false); show(listCard, false); show(forgetBtn, false);
    tokenInput.disabled = false; unlockBtn.disabled = false;
    setStatus('Locked. The token is the HOP_TOKEN value — see “Where is my token” below.');
  }
  function unlocked() {
    show(form, true); show(listCard, true); show(forgetBtn, true);
    tokenInput.value = '••••••••••••'; tokenInput.disabled = true; unlockBtn.disabled = true;
    setStatus('Unlocked — the token is stored in this browser only.', 'ok');
    loadList();
  }
  function unlock(t) {
    t = (t || '').trim();
    if (!t) { setStatus('Enter the token first.', 'err'); return; }
    token = t;
    setStatus('Checking…');
    api('GET', '/api/' + kind).then(function (res) {
      if (res.ok) { saveToken(t); unlocked(); }
      else { token = ''; setStatus(errText(res), 'err'); }
    }).catch(function () { token = ''; setStatus('network error', 'err'); });
  }
  unlockBtn.addEventListener('click', function () { unlock(tokenInput.value); });
  tokenInput.addEventListener('keydown', function (e) { if (e.key === 'Enter') { e.preventDefault(); unlock(tokenInput.value); } });
  forgetBtn.addEventListener('click', lock);
  if (token) { unlock(token); }

  // --- create --------------------------------------------------------------------
  function showError(msg) { createErr.textContent = msg; show(createErr, !!msg); }
  function showResult(url, meta) {
    resultURL.textContent = url; resultURL.href = url; resultMeta.textContent = meta || '';
    show(resultCard, true); resultCard.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }
  copyResult.addEventListener('click', function () { copy(resultURL.textContent, copyResult); });

  if (kind === 'links') {
    var lURL = $('l-url'), lSlug = $('l-slug'), lTTL = $('l-ttl');
    form.addEventListener('submit', function (e) {
      e.preventDefault(); showError('');
      var payload = { url: lURL.value.trim() };
      if (lSlug.value.trim()) payload.slug = lSlug.value.trim();
      if (lTTL.value) payload.ttl = lTTL.value;
      var btn = form.querySelector('button[type=submit]'); btn.disabled = true;
      api('POST', '/api/links', { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) })
        .then(function (res) {
          btn.disabled = false;
          if (!res.ok) { showError(errText(res)); return; }
          showResult(res.data.short_url, '→ ' + res.data.url + (res.data.expires_at ? ' · expires ' + fmtDate(res.data.expires_at) : ' · never expires'));
          lURL.value = ''; lSlug.value = ''; loadList();
        }).catch(function () { btn.disabled = false; showError('network error'); });
    });
  } else {
    var pTitle = $('p-title'), pLang = $('p-lang'), pTTL = $('p-ttl'), pContent = $('p-content'), pSize = $('p-size');
    var updSize = function () { pSize.textContent = fmtSize(new Blob([pContent.value]).size); };
    pContent.addEventListener('input', updSize);
    form.addEventListener('submit', function (e) {
      e.preventDefault(); showError('');
      if (!pContent.value) { showError('nothing to paste'); return; }
      var btn = form.querySelector('button[type=submit]'); btn.disabled = true;
      api('POST', '/api/pastes', {
        headers: { 'Content-Type': 'text/plain; charset=utf-8', 'X-Title': pTitle.value.trim(), 'X-Lang': pLang.value, 'X-TTL': pTTL.value },
        body: pContent.value
      }).then(function (res) {
        btn.disabled = false;
        if (!res.ok) { showError(errText(res)); return; }
        showResult(res.data.url, fmtSize(res.data.size) + (res.data.expires_at ? ' · expires ' + fmtDate(res.data.expires_at) : ' · never expires') + ' · raw: ' + res.data.raw_url);
        pContent.value = ''; pTitle.value = ''; updSize(); loadList();
      }).catch(function () { btn.disabled = false; showError('network error'); });
    });
  }

  // --- list ----------------------------------------------------------------------
  function loadList() {
    api('GET', '/api/' + kind).then(function (res) {
      if (!res.ok) { if (res.status === 401) lock(); return; }
      render(Array.isArray(res.data) ? res.data : []);
    }).catch(function () { /* keep old list */ });
  }
  function actionCell(buttons) {
    var td = el('td', { class: 'actions' });
    buttons.forEach(function (b) { td.appendChild(b); });
    return td;
  }
  function btn(text, cls, onClick) { var b = el('button', { type: 'button', class: 'btn small ' + (cls || ''), text: text }); b.addEventListener('click', onClick); return b; }
  function render(items) {
    listBody.textContent = '';
    show(listEmpty, items.length === 0);
    var host = location.origin;
    items.forEach(function (it) {
      var tr = el('tr');
      if (kind === 'links') {
        var url = host + '/' + it.slug;
        tr.appendChild(el('td', {}, [el('a', { href: url, target: '_blank', rel: 'noopener', text: '/' + it.slug })]));
        tr.appendChild(el('td', { class: 'dim', title: it.url, text: trunc(it.url, 60) }));
        tr.appendChild(el('td', { text: String(it.hits || 0) }));
        tr.appendChild(el('td', { class: 'dim', text: it.expires_at ? fmtDate(it.expires_at) : 'never' }));
        tr.appendChild(actionCell([
          btn('Copy', 'ghost', function (e) { copy(url, e.target); }),
          btn('Delete', 'danger', function () { if (confirm('Delete /' + it.slug + '?')) api('DELETE', '/api/links/' + encodeURIComponent(it.slug)).then(loadList); })
        ]));
      } else {
        var purl = host + '/' + it.id;
        tr.appendChild(el('td', {}, [el('a', { href: purl, target: '_blank', rel: 'noopener', text: it.title || it.id }), el('span', { class: 'dim small', text: it.title ? ' ' + it.id : '' })]));
        tr.appendChild(el('td', { class: 'dim', text: it.lang || 'text' }));
        tr.appendChild(el('td', { text: fmtSize(it.size || 0) }));
        tr.appendChild(el('td', { class: 'dim', text: fmtDate(it.created_at) }));
        tr.appendChild(el('td', { class: 'dim', text: it.expires_at ? fmtDate(it.expires_at) : 'never' }));
        tr.appendChild(actionCell([
          btn('Open', 'ghost', function () { window.open(purl + '?html=1', '_blank', 'noopener'); }),
          btn('Copy', 'ghost', function (e) { copy(purl, e.target); }),
          btn('Delete', 'danger', function () { if (confirm('Delete paste ' + it.id + '?')) api('DELETE', '/api/pastes/' + encodeURIComponent(it.id)).then(loadList); })
        ]));
      }
      listBody.appendChild(tr);
    });
  }
  if (refreshBtn) refreshBtn.addEventListener('click', loadList);
})();
