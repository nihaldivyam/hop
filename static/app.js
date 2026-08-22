// hop landing UI — vanilla JS, no dependencies. Talks only to this origin's /api/*.
// The token lives in localStorage ("hop.token") and is sent as a Bearer header.
(function () {
  'use strict';
  var body = document.body;
  var kind = body.getAttribute('data-kind') === 'pastes' ? 'pastes' : 'links';
  // public pastes mode: the paste host accepts anonymous creates (small, short-lived)
  var publicPastes = kind === 'pastes' && body.getAttribute('data-public-pastes') === '1';
  // public links mode: the links host accepts anonymous creates (random slug, short-lived)
  var publicLinks = kind === 'links' && body.getAttribute('data-public-links') === '1';
  var isPublic = publicPastes || publicLinks;
  var anonMax = parseInt(body.getAttribute('data-anon-max-bytes') || '0', 10) || 0;
  var anonTTL = body.getAttribute('data-anon-ttl') || '';
  var KEY = 'hop.token';
  var $ = function (id) { return document.getElementById(id); };
  var tokenInput = $('token'), unlockBtn = $('unlock'), forgetBtn = $('forget'), tokenStatus = $('token-status');
  var form = $('create-form'), createErr = $('create-error');
  var resultCard = $('result-card'), resultURL = $('result-url'), resultMeta = $('result-meta'), copyResult = $('copy-result');
  var listCard = $('list-card'), listBody = document.querySelector('#list tbody'), listEmpty = $('list-empty'), refreshBtn = $('refresh');
  var anonBadge = $('anon-badge'), ttlWrap = $('p-ttl-wrap'), tokenDetails = $('token-details');
  var lSlugWrap = $('l-slug-wrap'), lTTLWrap = $('l-ttl-wrap'), lAnonNote = $('l-anon-note');
  // "name your own URL": the create page (GET /<free-name>) pre-fills the custom id and
  // navigates to the paste once it exists instead of showing the result card
  var createID = body.getAttribute('data-create-id') || '';
  var pID = $('p-id');
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
    var h = {};
    if (token) h['Authorization'] = 'Bearer ' + token; // anonymous pastes send no token at all
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
    var d = res.data || {};
    if (res.status === 401) return 'token rejected — check it and unlock again';
    if (res.status === 503) return 'writes are disabled on this instance';
    if (res.status === 429) {
      if (d.retry_after_seconds) return 'rate limited — try again in ' + humanWait(d.retry_after_seconds) + (token ? '' : ' (or unlock with the token)');
      return d.error === 'daily cap reached' ? 'the anonymous budget for today is used up — try tomorrow or unlock with the token' : 'slow down — rate limited';
    }
    if (res.status === 413) return d.error || ('too large' + (anonMax ? ' — anonymous pastes are limited to ' + fmtSize(anonMax) : ''));
    if (res.status === 415) return d.error || 'anonymous pastes must be plain text';
    if (res.status === 409) return (d.error === 'id taken' ? 'that name is already taken — pick another one' : d.error || 'already exists');
    return d.error ? d.error : ('request failed (HTTP ' + res.status + ')');
  }
  function humanWait(sec) {
    sec = Math.max(1, Math.round(sec));
    if (sec < 90) return sec + ' s';
    if (sec < 5400) return Math.round(sec / 60) + ' min';
    return (sec / 3600).toFixed(1) + ' h';
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
  function anonMode() {
    // public pastes/links: the form stays usable without a token, within the anonymous limits
    // (on the create page the form is always shown; without public mode it needs an unlock first)
    show(form, isPublic || !!createID); show(anonBadge, isPublic); show(ttlWrap, !publicPastes);
    show(lSlugWrap, !publicLinks); show(lTTLWrap, !publicLinks); show(lAnonNote, publicLinks);
    show(resultCard, false); show(listCard, false); show(forgetBtn, false);
    if (publicPastes && typeof updSize === 'function') updSize();
  }
  function lock() {
    saveToken('');
    tokenInput.value = '';
    anonMode();
    tokenInput.disabled = false; unlockBtn.disabled = false;
    setStatus('Locked. The token is the HOP_TOKEN value — see “Where is my token” below.');
  }
  function unlocked() {
    show(form, true); show(listCard, true); show(forgetBtn, true);
    show(anonBadge, false); show(ttlWrap, true);
    show(lSlugWrap, true); show(lTTLWrap, true); show(lAnonNote, false);
    if (tokenDetails) tokenDetails.open = true;
    tokenInput.value = '••••••••••••'; tokenInput.disabled = true; unlockBtn.disabled = true;
    setStatus('Unlocked — full limits, and your ' + kind + ' are listed below. The token is stored in this browser only.', 'ok');
    if (typeof updSize === 'function') updSize();
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

  // --- create --------------------------------------------------------------------
  function showError(msg) { createErr.textContent = msg; show(createErr, !!msg); }
  function showResult(url, meta) {
    resultURL.textContent = url; resultURL.href = url; resultMeta.textContent = meta || '';
    show(resultCard, true); resultCard.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }
  copyResult.addEventListener('click', function () { copy(resultURL.textContent, copyResult); });

  if (kind === 'links') {
    var lURL = $('l-url'), lSlug = $('l-slug'), lTTL = $('l-ttl');
    var isAnonLink = function () { return publicLinks && !token; };
    form.addEventListener('submit', function (e) {
      e.preventDefault(); showError('');
      var payload = { url: lURL.value.trim() };
      // anonymous links: random slug and the fixed short TTL, decided by the server
      if (!isAnonLink() && lSlug.value.trim()) payload.slug = lSlug.value.trim();
      if (!isAnonLink() && lTTL.value) payload.ttl = lTTL.value;
      var btn = form.querySelector('button[type=submit]'); btn.disabled = true;
      api('POST', '/api/links', { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) })
        .then(function (res) {
          btn.disabled = false;
          if (!res.ok) { showError(errText(res)); return; }
          showResult(res.data.short_url, '→ ' + res.data.url + (res.data.expires_at ? ' · expires ' + fmtDate(res.data.expires_at) : ' · never expires') + (res.data.anon ? ' · anonymous (visitors see a confirmation page first)' : ''));
          lURL.value = ''; lSlug.value = ''; if (token) loadList();
        }).catch(function () { btn.disabled = false; showError('network error'); });
    });
  } else {
    var pTitle = $('p-title'), pLang = $('p-lang'), pTTL = $('p-ttl'), pContent = $('p-content'), pSize = $('p-size');
    var isAnon = function () { return publicPastes && !token; };
    var updSize = function () {
      var n = new Blob([pContent.value]).size;
      var over = isAnon() && anonMax && n > anonMax;
      pSize.textContent = fmtSize(n) + (isAnon() && anonMax ? ' / ' + fmtSize(anonMax) + (over ? ' — too large for an anonymous paste' : '') : '');
      pSize.className = 'muted small' + (over ? ' over' : '');
    };
    pContent.addEventListener('input', updSize);
    form.addEventListener('submit', function (e) {
      e.preventDefault(); showError('');
      if (!pContent.value) { showError('nothing to paste'); return; }
      if (isAnon() && anonMax && new Blob([pContent.value]).size > anonMax) { showError('anonymous pastes are limited to ' + fmtSize(anonMax) + ' — shorten it or unlock with the token'); return; }
      var btn = form.querySelector('button[type=submit]'); btn.disabled = true;
      var hdr = { 'Content-Type': 'text/plain; charset=utf-8', 'X-Title': pTitle.value.trim(), 'X-Lang': pLang.value };
      if (!isAnon()) hdr['X-TTL'] = pTTL.value; // anonymous pastes always get the fixed short TTL
      var customID = (createID || (pID ? pID.value.trim() : ''));
      if (customID) {
        if (!/^[A-Za-z0-9][A-Za-z0-9_-]{0,14}$/.test(customID)) { btn.disabled = false; showError('custom URL: 1–15 letters/digits/-/_, starting with a letter or digit'); return; }
        hdr['X-Id'] = customID;
      }
      api('POST', '/api/pastes', { headers: hdr, body: pContent.value }).then(function (res) {
        btn.disabled = false;
        if (!res.ok) { showError(errText(res)); return; }
        if (createID) { location.assign('/' + encodeURIComponent(res.data.id)); return; } // the page becomes the paste
        showResult(res.data.url, fmtSize(res.data.size) + (res.data.expires_at ? ' · expires ' + fmtDate(res.data.expires_at) : ' · never expires') + (res.data.anon ? ' · anonymous' : '') + ' · raw: ' + res.data.raw_url);
        pContent.value = ''; pTitle.value = ''; if (pID) pID.value = ''; updSize(); if (token) loadList();
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

  // initial state: a stored token unlocks; otherwise anonymous mode (public pastes) or locked
  if (token) { unlock(token); } else { anonMode(); }
})();
