// tailfuse.com's measurement, by ailio.ca's consent model (src/worker.js, /data/consent; ailio.ca's
// apps/ailio/public/consent.js): measure first, ask later. Outside Quebec an unanswered visitor is measured with
// Google Analytics from the first page and sees the bar; Quebec (Law 25) waits for "Allow"; a "No" stops it and
// removes Google's cookies. Ad signals are always denied. The one event is a download click (ga4_key_events in
// Terraform); no address, query string or typed value is sent.
(function () {
  'use strict';

  if (location.hostname === 'localhost' || location.hostname === '127.0.0.1') return;

  // A No takes back what an earlier visit stored: Google's cookies, before the page reloads without them.
  function forget() {
    document.cookie.split(';').forEach(function (part) {
      var name = part.split('=')[0].trim();
      if (!/^_ga/.test(name)) return;
      ['', '; domain=' + location.hostname, '; domain=.' + location.hostname].forEach(function (domain) {
        document.cookie = name + '=; Max-Age=0; path=/' + domain;
      });
    });
  }

  function answer(choice) {
    if (choice === 'denied') forget();
    return fetch('/data/consent', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ choice: choice }),
      credentials: 'same-origin'
    }).then(function (r) {
      if (!r.ok) throw new Error('consent ' + r.status);
      location.reload();
    });
  }

  function loadGoogle(id) {
    window.dataLayer = window.dataLayer || [];
    window.gtag = function () { window.dataLayer.push(arguments); };
    window.gtag('consent', 'default', {
      ad_storage: 'denied',
      ad_user_data: 'denied',
      ad_personalization: 'denied',
      analytics_storage: 'granted'
    });
    window.gtag('js', new Date());
    window.gtag('config', id, { anonymize_ip: true });
    var script = document.createElement('script');
    script.async = true;
    script.src = 'https://www.googletagmanager.com/gtag/js?id=' + encodeURIComponent(id);
    document.head.appendChild(script);
    document.addEventListener('click', function (event) {
      var link = event.target.closest && event.target.closest('a[href^="/download"]');
      if (link) window.gtag('event', 'download_click');
    });
  }

  function el(tag, props, children) {
    var node = document.createElement(tag);
    Object.keys(props || {}).forEach(function (k) { node.setAttribute(k, props[k]); });
    (children || []).forEach(function (c) { node.appendChild(typeof c === 'string' ? document.createTextNode(c) : c); });
    return node;
  }

  // The same bar and words as ailio.ca and the other Ailio sites: No / Allow.
  function showBanner() {
    var bar = el('div', { class: 'consent', role: 'region', 'aria-label': 'Measurement' }, [
      el('p', {}, ['This website uses cookies to ensure you get the best experience on our website. ', el('a', { href: '/privacy' }, ['Privacy'])]),
      el('div', { class: 'choices' }, [
        el('button', { type: 'button', 'data-choice': 'denied' }, ['No']),
        el('button', { type: 'button', class: 'primary', 'data-choice': 'granted' }, ['Allow'])
      ])
    ]);
    bar.addEventListener('click', function (event) {
      var choice = event.target.getAttribute && event.target.getAttribute('data-choice');
      if (choice) answer(choice).catch(function () { bar.remove(); release(); });
    });
    document.body.appendChild(bar);

    // The bar is fixed over the bottom of the page. While it is up, the page is padded by the space it covers
    // (its height and the gap below it, plus a gap above), so the footer's links can still be scrolled clear of it.
    function reserve() {
      var covered = window.innerHeight - bar.getBoundingClientRect().top;
      document.body.style.setProperty('padding-bottom', Math.max(0, Math.ceil(covered) + 16) + 'px');
    }
    function release() {
      window.removeEventListener('resize', reserve);
      if (observer) observer.disconnect();
      document.body.style.removeProperty('padding-bottom');
    }
    var observer = window.ResizeObserver ? new ResizeObserver(reserve) : null;
    if (observer) observer.observe(bar);
    window.addEventListener('resize', reserve);
    reserve();
  }

  fetch('/data/consent', { credentials: 'same-origin' })
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (state) {
      // No answer from our own route: measure nothing and ask nothing.
      if (!state) return;
      // An answer given under the old bar (kept in this browser) carries over once, without asking again.
      var old = null;
      try { old = localStorage.getItem('tailfuse-consent'); localStorage.removeItem('tailfuse-consent'); } catch (e) {}
      if (state.banner && (old === 'allow' || old === 'decline')) { answer(old === 'allow' ? 'granted' : 'denied'); return; }
      if (state.analytics && state.gaId) loadGoogle(state.gaId);
      if (state.banner) {
        if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', showBanner);
        else showBanner();
      }
    })
    .catch(function () {});
})();
