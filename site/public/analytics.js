// Google Analytics for tailfuse.com, loaded only with the visitor's consent.
// The measurement id is public by design (Terraform output); the choice is
// remembered in localStorage. Mirrors dagpeek's site.js, without the server
// consent endpoint: a self-contained banner,
// worded and laid out like ailio.ca's (No / Allow).
(function () {
  'use strict';

  var ID = 'G-B598JDM8JW'; // tailfuse_ga4_measurement_id (Terraform output); public by design.
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

  // The same bar and words as ailio.ca and the other Ailio sites: No / Allow.
  var banner = document.createElement('div');
  banner.className = 'consent';
  banner.setAttribute('role', 'region');
  banner.setAttribute('aria-label', 'Measurement');
  banner.innerHTML =
    '<p>This website uses cookies to ensure you get the best experience on our website. <a href="/privacy">Privacy</a></p>' +
    '<div class="choices">' +
    '<button type="button" data-consent="decline">No</button>' +
    '<button type="button" class="primary" data-consent="allow">Allow</button>' +
    '</div>';
  var show = function () { document.body.appendChild(banner); };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', show); else show();

  banner.addEventListener('click', function (event) {
    var pick = event.target.getAttribute && event.target.getAttribute('data-consent');
    if (!pick) return;
    try { localStorage.setItem(KEY, pick); } catch (e) { /* remember nothing */ }
    banner.remove();
    if (pick === 'allow') start();
  });
})();
