// Google Analytics for tailfuse.com, loaded only with the visitor's consent.
// The measurement id is public by design (Terraform output); the choice is
// remembered in localStorage. Mirrors dagpeek's site.js, without the server
// consent endpoint: a self-contained banner, Allow/Decline.
(function () {
  'use strict';

  var ID = 'G-XXXXXXXXXX'; // tailfuse_ga4_measurement_id (Terraform); filled after the apply.
  var KEY = 'tailfuse-consent';

  function start() {
    window.dataLayer = window.dataLayer || [];
    window.gtag = function () { window.dataLayer.push(arguments); };
    window.gtag('consent', 'default', {
      ad_storage: 'denied',
      ad_user_data: 'denied',
      ad_personalization: 'denied',
      analytics_storage: 'granted',
    });
    window.gtag('js', new Date());
    window.gtag('config', ID, { anonymize_ip: true });
    var script = document.createElement('script');
    script.async = true;
    script.src = 'https://www.googletagmanager.com/gtag/js?id=' + ID;
    document.head.appendChild(script);
  }

  // Download clicks are the one key event (ga4_key_events in Terraform).
  document.addEventListener('click', function (event) {
    var link = event.target.closest && event.target.closest('a[href^="/download"]');
    if (link && window.gtag) window.gtag('event', 'download_click');
  });

  var choice = null;
  try { choice = localStorage.getItem(KEY); } catch (e) { /* private mode: ask each time */ }

  if (choice === 'allow') {
    start();
    return;
  }
  if (choice === 'decline') return;

  var banner = document.createElement('div');
  banner.setAttribute('role', 'dialog');
  banner.setAttribute('aria-label', 'Analytics consent');
  banner.innerHTML =
    '<p>Allow anonymous usage statistics? Nothing identifying leaves this page.</p>' +
    '<button type="button" data-consent="allow">Allow</button>' +
    '<button type="button" data-consent="decline">Decline</button>';
  document.addEventListener('DOMContentLoaded', function () { document.body.appendChild(banner); });

  banner.addEventListener('click', function (event) {
    var pick = event.target.getAttribute && event.target.getAttribute('data-consent');
    if (!pick) return;
    try { localStorage.setItem(KEY, pick); } catch (e) { /* remember nothing */ }
    banner.remove();
    if (pick === 'allow') start();
  });
})();
