// tailfuse.com: the routing tester, the app window's power switch and the two type moments.
(function () {
  'use strict';

  var reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  var clamp = function (v) { return Math.max(0, Math.min(1, v)); };

  // Routing tester. Mirrors the router's rule: the longest owned suffix wins, anything else is refused.
  var SUFFIXES = [
    ['.home-lab.ts.net', 'personal'],
    ['.work.ts.net', 'work'],
    ['.internal.example', 'work']
  ];

  function owner(raw) {
    var host = String(raw || '').trim().toLowerCase()
      .replace(/^[a-z0-9+.-]+:\/\//, '').split('/')[0].split(':')[0];
    var best = null;
    SUFFIXES.forEach(function (row) {
      var s = row[0];
      if (host.length > s.length && host.slice(-s.length) === s && (!best || s.length > best[0].length)) best = row;
    });
    return { host: host, profile: best ? best[1] : null };
  }

  var input = document.getElementById('host');
  var cmd = document.getElementById('test-cmd');
  var out = document.getElementById('test-out');
  var lanes = Array.prototype.slice.call(document.querySelectorAll('.lane'));
  var picks = Array.prototype.slice.call(document.querySelectorAll('.pick'));

  function route() {
    var res = owner(input.value);
    var typed = res.host.length > 0;
    var lit = !typed ? '' : (res.profile || 'none');
    lanes.forEach(function (lane) {
      var on = lane.getAttribute('data-lane') === lit;
      if (on && lane.classList.contains('is-on')) return;
      lane.classList.toggle('is-on', on);
    });
    picks.forEach(function (b) {
      b.setAttribute('aria-pressed', String(b.getAttribute('data-host') === res.host));
    });
    cmd.textContent = 'tailfuse test ' + (typed ? res.host : '<host>');
    if (!typed) out.textContent = 'Type a hostname to see which profile owns it.';
    else if (res.profile) out.textContent = res.host + ' → profile "' + res.profile + '"';
    else out.textContent = res.host + ' → not owned by any profile (would be refused)';
  }

  if (input && cmd && out) {
    input.addEventListener('input', route);
    picks.forEach(function (b) {
      b.addEventListener('click', function () {
        input.value = b.getAttribute('data-host');
        route();
      });
    });
    route();
  }

  // The power switch in the app window.
  var win = document.getElementById('app-window');
  var power = document.getElementById('power');
  var powerLabel = document.getElementById('power-label');
  if (win && power && powerLabel) {
    power.addEventListener('click', function () {
      var running = win.getAttribute('data-running') !== 'true';
      win.setAttribute('data-running', String(running));
      power.setAttribute('aria-checked', String(running));
      powerLabel.textContent = running ? 'Running' : 'Stopped';
      Array.prototype.forEach.call(win.querySelectorAll('.card__pill'), function (p) {
        p.textContent = running ? 'online' : 'stopped';
      });
    });
  }

  // The license form. $0 returns a key at once; any other amount goes through Stripe Checkout first.
  var form = document.getElementById('donate');
  if (form) {
    var range = document.getElementById('amount-range');
    var box = document.getElementById('amount-box');
    var amountOut = document.getElementById('amount-out');
    var email = document.getElementById('email');
    var go = document.getElementById('donate-go');
    var note = document.getElementById('donate-note');
    var result = document.getElementById('donate-result');
    var keyBox = document.getElementById('key-box');
    var keyEmail = document.getElementById('key-email');
    var keyCopy = document.getElementById('key-copy');
    var MAX = 1000;
    var canDonate = true;
    var busy = false;

    var amount = function () {
      var n = Math.floor(Number(box.value));
      return isFinite(n) && n > 0 ? Math.min(MAX, n) : 0;
    };
    var say = function (text, isError) {
      note.textContent = text;
      note.classList.toggle('is-error', Boolean(isError));
    };
    var paint = function () {
      var n = amount();
      amountOut.textContent = String(n);
      range.value = String(Math.min(10, n));
      go.textContent = n > 0 ? 'Donate $' + n + ' and get a key' : 'Get a free license key';
    };
    var showKey = function (data) {
      keyBox.value = data.key;
      keyEmail.textContent = data.email || '';
      result.hidden = false;
      say(data.emailed ? 'Thank you. The key is also on its way to ' + (data.email || 'your inbox') + '.' : 'Keep this key somewhere safe.', false);
      result.scrollIntoView({ block: 'nearest', behavior: reduced ? 'auto' : 'smooth' });
    };
    var api = function (path, options) {
      return fetch(path, options).then(function (res) {
        return res.json().catch(function () { return {}; }).then(function (data) {
          if (!res.ok) throw new Error(data.error || 'Something went wrong. Try again.');
          return data;
        });
      });
    };

    range.addEventListener('input', function () {
      box.value = range.value;
      paint();
    });
    box.addEventListener('input', paint);
    box.addEventListener('blur', function () {
      box.value = String(amount());
      paint();
    });

    form.addEventListener('submit', function (e) {
      e.preventDefault();
      if (busy) return;
      var n = amount();
      var address = email.value.trim();
      if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(address)) {
        say('Enter the email address to put on the license.', true);
        email.focus();
        return;
      }
      if (n > 0 && !canDonate) {
        say('Donations are not open yet. Set the amount to $0 for the same key.', true);
        return;
      }
      busy = true;
      go.disabled = true;
      say(n > 0 ? 'Opening the Stripe payment page.' : 'Signing your key.', false);
      api('/api/license', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ email: address, amount: n })
      }).then(function (data) {
        if (data.url) {
          window.location.assign(data.url);
          return;
        }
        showKey(data);
      }).catch(function (err) {
        say(err.message, true);
      }).then(function () {
        busy = false;
        go.disabled = false;
      });
    });

    keyCopy.addEventListener('click', function () {
      var done = function () {
        keyCopy.textContent = 'Copied';
        window.setTimeout(function () { keyCopy.textContent = 'Copy the key'; }, 1600);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(keyBox.value).then(done, function () { keyBox.select(); });
      } else {
        keyBox.select();
        try { document.execCommand('copy'); done(); } catch (err) { /* the key stays selected for a manual copy */ }
      }
    });

    paint();

    // Back from Stripe: the session id in the address buys the key.
    var params = new URLSearchParams(window.location.search);
    var session = params.get('session_id');
    if (session) {
      say('Confirming your donation.', false);
      api('/api/license?session_id=' + encodeURIComponent(session)).then(showKey).catch(function (err) {
        say(err.message, true);
      });
      window.history.replaceState(null, '', window.location.pathname + '#pricing');
    }

    api('/api/status').then(function (status) {
      if (!status.licenses) {
        go.disabled = true;
        busy = true;
        say('License keys are not being issued yet.', true);
      } else if (!status.donations) {
        canDonate = false;
        if (!session) {
          box.value = '0';
          paint();
          say('Donations are not open yet. A $0 key is the same key.', false);
        }
      }
    }).catch(function () { /* leave the form as it is; a submit will report the problem */ });
  }

  // The address assembles: each character starts scattered and lands as the band scrolls into view.
  var addr = document.querySelector('.addr');
  if (addr && !reduced && window.matchMedia('(min-width: 640px)').matches) {
    var chars = Array.prototype.slice.call(addr.children);
    var SEEDS = [
      [-1.1, 1.2, -32], [0.7, -1.5, 24], [-0.4, 1.7, -41], [1.2, 0.8, 18], [-0.9, -1.1, 36],
      [0.3, 1.4, -22], [1.4, -0.7, -44], [-1.3, 0.9, 29], [0.6, 1.8, -15], [-0.2, -1.6, 40],
      [1.1, 1.1, -34], [-0.8, -0.9, 21], [0.9, 1.5, -27], [-1.4, -1.3, 45], [0.5, 0.6, -38]
    ];
    var n = chars.length;
    var queued = false;
    var place = function () {
      queued = false;
      var vh = window.innerHeight;
      var t = clamp((vh - addr.getBoundingClientRect().top) / (vh * 0.5));
      chars.forEach(function (c, i) {
        var k = SEEDS[i % SEEDS.length];
        var at = (i / Math.max(1, n - 1)) * 0.5;
        var p = clamp((t - at * 0.6) / 0.4);
        var e = 1 - Math.pow(1 - p, 5);
        var u = 1 - e;
        if (u < 0.002) {
          c.style.transform = '';
          c.style.opacity = '';
        } else {
          c.style.transform = 'translate(' + (k[0] * u).toFixed(3) + 'em,' + (k[1] * u).toFixed(3) + 'em) rotate(' + (k[2] * u).toFixed(2) + 'deg)';
          c.style.opacity = (0.08 + 0.92 * e).toFixed(3);
        }
      });
    };
    var ask = function () {
      if (!queued) {
        queued = true;
        window.requestAnimationFrame(place);
      }
    };
    window.addEventListener('scroll', ask, { passive: true });
    window.addEventListener('resize', ask);
    place();
  }

  // The closing line ripples under the pointer and settles within about a second. One SVG filter, on only while it moves.
  var type = document.getElementById('close-type');
  var turb = document.querySelector('#liquid feTurbulence');
  var disp = document.querySelector('#liquid feDisplacementMap');
  if (type && turb && disp && !reduced && !window.matchMedia('(pointer: coarse)').matches) {
    var energy = 0, raf = 0, on = false, lastX = 0, lastY = 0, has = false, px = 0.5;
    var tick = function (now) {
      raf = 0;
      energy *= 0.93;
      if (energy < 0.6) {
        energy = 0;
        if (on) {
          type.style.filter = '';
          on = false;
        }
        return;
      }
      disp.setAttribute('scale', energy.toFixed(1));
      turb.setAttribute('baseFrequency', (0.006 + (px - 0.5) * 0.002).toFixed(4) + ' ' + (0.013 + Math.sin(now / 700) * 0.003).toFixed(4));
      if (!on) {
        type.style.filter = 'url(#liquid)';
        on = true;
      }
      raf = window.requestAnimationFrame(tick);
    };
    type.addEventListener('pointermove', function (e) {
      var r = type.getBoundingClientRect();
      px = clamp((e.clientX - r.left) / Math.max(1, r.width));
      if (has) energy = Math.min(44, energy + Math.min(9, Math.hypot(e.clientX - lastX, e.clientY - lastY) * 0.3) * 0.8);
      lastX = e.clientX;
      lastY = e.clientY;
      has = true;
      if (!raf) raf = window.requestAnimationFrame(tick);
    }, { passive: true });
    type.addEventListener('pointerleave', function () { has = false; });
  }
})();
