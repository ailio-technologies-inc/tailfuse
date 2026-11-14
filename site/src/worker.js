// tailfuse.com: serves the static site and issues license keys.
//
// A license key is free. A donation is optional: $0 signs a key straight away, any other amount
// goes through Stripe Checkout first and the key is signed once the session reads as paid.
// Nothing is stored: a paid key is derived from its Stripe session, so the same session always
// returns the same key.
//
// Secrets (wrangler secret put <NAME>):
//   LICENSE_PRIVKEY    base64url Ed25519 private key, the `private:` line of `licgen --gen-keys`.
//                      Its public half is PUBKEY in crates/tailfuse-core/src/license.rs.
//   STRIPE_SECRET_KEY  Stripe secret or restricted key (Checkout Sessions: write).
// Local runs only (.dev.vars): SITE_ORIGIN, the dev server's own address.
//   STRIPE_WEBHOOK_SECRET  signing secret of the Stripe webhook that posts to /api/stripe-webhook.
// Binding: EMAIL (send_email), used to email a donor their key from license@tailfuse.com.

const MAX_DONATION_USD = 1000;
const YEAR_MS = 365 * 24 * 60 * 60 * 1000;
// DER header of a PKCS#8 Ed25519 private key; the 32-byte seed follows it.
const PKCS8_ED25519 = [0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x04, 0x22, 0x04, 0x20];

const json = (body, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store' },
  });

function b64urlDecode(text) {
  const b64 = text.replace(/-/g, '+').replace(/_/g, '/');
  const bin = atob(b64 + '='.repeat((4 - (b64.length % 4)) % 4));
  return Uint8Array.from(bin, (c) => c.charCodeAt(0));
}

function b64urlEncode(bytes) {
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function cleanEmail(value) {
  const email = String(value ?? '').trim();
  return email.length <= 254 && /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email) ? email : null;
}

// Same shape and wire format as crates/licgen: tailfuse_<payload>__<signature>, both base64url.
export async function signLicense(privkey, email, issuedAt) {
  const seed = b64urlDecode(String(privkey).trim());
  if (seed.length !== 32) throw new Error('LICENSE_PRIVKEY must be a 32-byte base64url key');
  const pkcs8 = new Uint8Array(PKCS8_ED25519.length + seed.length);
  pkcs8.set(PKCS8_ED25519);
  pkcs8.set(seed, PKCS8_ED25519.length);
  const key = await crypto.subtle.importKey('pkcs8', pkcs8, { name: 'Ed25519' }, false, ['sign']);
  const payload = new TextEncoder().encode(
    JSON.stringify({
      email,
      max_profiles: 0,
      updates_until: new Date(issuedAt.getTime() + YEAR_MS).toISOString().slice(0, 10),
      issued_at: issuedAt.toISOString(),
      key_id: `tf-${Math.floor(issuedAt.getTime() / 1000)}`,
    }),
  );
  const signature = new Uint8Array(await crypto.subtle.sign('Ed25519', key, payload));
  return `tailfuse_${b64urlEncode(payload)}__${b64urlEncode(signature)}`;
}

const escapeHtml = (text) => String(text).replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);

// Emails a donor their key. A failed send never blocks the key itself: the page still shows it.
async function emailKey(env, email, key, updatesUntil) {
  if (!env.EMAIL) return false;
  const text = [
    'Thank you for donating to Tailfuse.',
    '',
    'Your license key:',
    '',
    key,
    '',
    'To use it, open Tailfuse and paste the key into the License field, or run:',
    '',
    '  tailfuse license activate <key>',
    '',
    `The key is issued to ${email} and covers updates until ${updatesUntil}. The version you have keeps working after that.`,
    'It is verified on your machine: no account and no phone-home.',
    '',
    'Questions? Reply to this email.',
    '',
    'Tailfuse, https://tailfuse.com',
  ].join('\n');
  const html = `<div style="font-family: -apple-system, 'Segoe UI', sans-serif; font-size: 16px; line-height: 1.5; color: #18191b; max-width: 600px;">
<p>Thank you for donating to Tailfuse.</p>
<p>Your license key:</p>
<pre style="white-space: pre-wrap; word-break: break-all; background: #f1f1f2; border-radius: 6px; padding: 14px; font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 13px;">${escapeHtml(key)}</pre>
<p>To use it, open Tailfuse and paste the key into the License field, or run:</p>
<pre style="background: #f1f1f2; border-radius: 6px; padding: 14px; font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 13px;">tailfuse license activate &lt;key&gt;</pre>
<p>The key is issued to ${escapeHtml(email)} and covers updates until ${escapeHtml(updatesUntil)}. The version you have keeps working after that. It is verified on your machine: no account and no phone-home.</p>
<p>Questions? Reply to this email.</p>
<p><a href="https://tailfuse.com" style="color: #c8340c;">tailfuse.com</a></p>
</div>`;
  try {
    await env.EMAIL.send({
      to: email,
      from: { email: 'license@tailfuse.com', name: 'Tailfuse' },
      replyTo: 'hello@ailio.ca',
      subject: 'Your Tailfuse license key',
      text,
      html,
    });
    return true;
  } catch (err) {
    console.error('license email failed', err?.code, err?.message);
    return false;
  }
}

async function stripe(env, path, params) {
  const res = await fetch(`https://api.stripe.com/v1/${path}`, {
    method: params ? 'POST' : 'GET',
    headers: {
      authorization: `Bearer ${env.STRIPE_SECRET_KEY}`,
      ...(params ? { 'content-type': 'application/x-www-form-urlencoded' } : {}),
    },
    body: params ? new URLSearchParams(params) : undefined,
  });
  const body = await res.json().catch(() => ({}));
  return { ok: res.ok, status: res.status, body };
}

// Where Stripe sends the donor back to: the public site. wrangler dev rewrites the request host to
// the route's, so a local run sets SITE_ORIGIN=http://localhost:<port> in .dev.vars instead.
function siteOrigin(env, url) {
  if (env.SITE_ORIGIN) return String(env.SITE_ORIGIN).replace(/\/+$/, '');
  return url.hostname === 'tailfuse.com' ? 'https://tailfuse.com' : url.origin;
}

// POST /api/license {email, amount}: amount is whole US dollars. 0 returns {key}; more returns {url}.
async function requestLicense(request, env, origin) {
  if (!env.LICENSE_PRIVKEY) return json({ error: 'License keys are not being issued yet.' }, 503);
  const input = await request.json().catch(() => null);
  const email = cleanEmail(input?.email);
  if (!email) return json({ error: 'Enter a valid email address.' }, 400);
  const amount = Number(input?.amount);
  if (!Number.isInteger(amount) || amount < 0 || amount > MAX_DONATION_USD) {
    return json({ error: `Enter a whole dollar amount from 0 to ${MAX_DONATION_USD}.` }, 400);
  }

  if (amount === 0) return json({ key: await signLicense(env.LICENSE_PRIVKEY, email, new Date()), email });

  if (!env.STRIPE_SECRET_KEY) return json({ error: 'Donations are not open yet. A $0 key is the same key.' }, 503);
  const session = await stripe(env, 'checkout/sessions', {
    mode: 'payment',
    submit_type: 'donate',
    customer_email: email,
    success_url: `${origin}/?session_id={CHECKOUT_SESSION_ID}#pricing`,
    cancel_url: `${origin}/#pricing`,
    'line_items[0][quantity]': '1',
    'line_items[0][price_data][currency]': 'usd',
    'line_items[0][price_data][unit_amount]': String(amount * 100),
    'line_items[0][price_data][product_data][name]': 'Tailfuse license (optional donation)',
    'metadata[product]': 'tailfuse-license',
    'payment_intent_data[receipt_email]': email,
  });
  if (!session.ok || !session.body.url) {
    console.error('stripe checkout create failed', session.status, session.body?.error?.message);
    return json({ error: 'The payment page could not be opened. Try again, or pick $0.' }, 502);
  }
  return json({ url: session.body.url });
}

// Emails the key for a paid session unless it already went out; the session's metadata remembers.
async function emailKeyOnce(env, session, email, key, issuedAt) {
  if (session.metadata?.key_emailed === '1') return true;
  const sent = await emailKey(env, email, key, new Date(issuedAt.getTime() + YEAR_MS).toISOString().slice(0, 10));
  if (sent) await stripe(env, `checkout/sessions/${session.id}`, { 'metadata[key_emailed]': '1' }).catch(() => {});
  return sent;
}

// Checks a Stripe-Signature header (t=<unix>,v1=<hex hmac of "t.body">) against the signing secret.
export async function verifyStripeSignature(secret, header, body, nowSeconds) {
  const parts = Object.fromEntries(String(header ?? '').split(',').map((p) => p.split('=').map((x) => x.trim())).filter((p) => p.length === 2 && p[0] !== 'v1'));
  const given = String(header ?? '').split(',').map((p) => p.trim()).filter((p) => p.startsWith('v1=')).map((p) => p.slice(3));
  const t = Number(parts.t);
  if (!Number.isFinite(t) || Math.abs(nowSeconds - t) > 300 || given.length === 0) return false;
  const key = await crypto.subtle.importKey('raw', new TextEncoder().encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
  const mac = new Uint8Array(await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(`${parts.t}.${body}`)));
  const want = [...mac].map((b) => b.toString(16).padStart(2, '0')).join('');
  // Constant-time comparison against each candidate.
  return given.some((sig) => {
    if (sig.length !== want.length) return false;
    let diff = 0;
    for (let i = 0; i < want.length; i += 1) diff |= sig.charCodeAt(i) ^ want.charCodeAt(i);
    return diff === 0;
  });
}

// POST /api/stripe-webhook: Stripe says a donation was paid, so the donor's key goes out by email
// whatever their browser does. A $0 key never reaches Stripe and is only shown on the page.
async function stripeWebhook(request, env) {
  if (!env.STRIPE_WEBHOOK_SECRET || !env.LICENSE_PRIVKEY || !env.STRIPE_SECRET_KEY) return json({ error: 'Webhook not configured.' }, 503);
  const body = await request.text();
  const ok = await verifyStripeSignature(env.STRIPE_WEBHOOK_SECRET, request.headers.get('stripe-signature'), body, Math.floor(Date.now() / 1000));
  if (!ok) return json({ error: 'Bad signature.' }, 400);
  let event;
  try {
    event = JSON.parse(body);
  } catch {
    return json({ error: 'Bad payload.' }, 400);
  }
  if (event.type !== 'checkout.session.completed' && event.type !== 'checkout.session.async_payment_succeeded') return json({ received: true });
  const id = event.data?.object?.id ?? '';
  if (!/^cs_[A-Za-z0-9_]{10,200}$/.test(id)) return json({ received: true });
  // Read the session back from Stripe: its current state, not the event's copy.
  const session = await stripe(env, `checkout/sessions/${id}`);
  if (!session.ok) return json({ error: 'Session lookup failed.' }, 502);
  const s = session.body;
  if (s.metadata?.product !== 'tailfuse-license' || s.payment_status !== 'paid') return json({ received: true });
  const email = cleanEmail(s.customer_details?.email ?? s.customer_email);
  if (!email) return json({ received: true });
  const issuedAt = new Date(s.created * 1000);
  const key = await signLicense(env.LICENSE_PRIVKEY, email, issuedAt);
  const sent = await emailKeyOnce(env, s, email, key, issuedAt);
  // A failed send answers 500 so Stripe tries the event again later.
  return sent ? json({ received: true, emailed: true }) : json({ error: 'Email not sent.' }, 500);
}

// GET /api/license?session_id=cs_...: the key for a paid Checkout session.
async function claimLicense(url, env) {
  if (!env.LICENSE_PRIVKEY || !env.STRIPE_SECRET_KEY) return json({ error: 'License keys are not being issued yet.' }, 503);
  const id = url.searchParams.get('session_id') ?? '';
  if (!/^cs_[A-Za-z0-9_]{10,200}$/.test(id)) return json({ error: 'Unknown payment session.' }, 400);
  const session = await stripe(env, `checkout/sessions/${id}`);
  if (!session.ok || session.body.metadata?.product !== 'tailfuse-license') {
    return json({ error: 'Unknown payment session.' }, 404);
  }
  if (session.body.payment_status !== 'paid') return json({ error: 'This donation has not completed.' }, 402);
  const email = cleanEmail(session.body.customer_details?.email ?? session.body.customer_email);
  if (!email) return json({ error: 'The payment has no email address on it.' }, 422);
  const issuedAt = new Date(session.body.created * 1000);
  const key = await signLicense(env.LICENSE_PRIVKEY, email, issuedAt);
  // Email the key once per donation; the session's own metadata remembers that it went out.
  // With the webhook set up, Stripe's own event sends the email (it arrives even if this page
  // never loads); otherwise this visit sends it, once.
  let emailed = Boolean(env.STRIPE_WEBHOOK_SECRET) || session.body.metadata?.key_emailed === '1';
  if (!emailed) emailed = await emailKeyOnce(env, session.body, email, key, issuedAt);
  return json({ key, email, emailed });
}

// Downloads redirect to the latest GitHub release asset, so GitHub serves the bytes.
//   /download                    the menu-bar app for the visitor's system
//   /download/app/<platform>     the menu-bar app: macos, windows, linux, linux-deb
//   /download/cli/<platform>     the command-line tool: macos, windows, linux-amd64, linux-arm64
const REPO = 'ailio-technologies-inc/tailfuse';
const RELEASES = `https://github.com/${REPO}/releases/latest`;
const APP_SUFFIX = {
  macos: '-macos-universal.dmg',
  windows: '-windows-x86_64-setup.exe',
  linux: '-linux-amd64.AppImage',
  'linux-deb': '-linux-amd64.deb',
};
const CLI_SUFFIX = {
  macos: '-macos-universal.tar.gz',
  windows: '-windows-x86_64.zip',
  'linux-amd64': '-linux-amd64.tar.gz',
  'linux-arm64': '-linux-arm64.tar.gz',
};

function platformFromUserAgent(ua) {
  if (/iphone|ipad|ipod|android/i.test(ua)) return null;
  if (/windows/i.test(ua)) return 'windows';
  if (/macintosh|mac os x/i.test(ua)) return 'macos';
  if (/linux|x11|cros/i.test(ua)) return /aarch64|arm64/i.test(ua) ? null : 'linux';
  return null;
}

async function latestAssets(ctx) {
  const api = `https://api.github.com/repos/${REPO}/releases/latest`;
  const cache = caches.default;
  let res = await cache.match(api);
  if (!res) {
    res = await fetch(api, { headers: { accept: 'application/vnd.github+json', 'user-agent': 'tailfuse-site' } });
    if (!res.ok) return [];
    res = new Response(res.body, { headers: { 'content-type': 'application/json', 'cache-control': 'public, max-age=300' } });
    ctx.waitUntil(cache.put(api, res.clone()));
  }
  const release = await res.json().catch(() => null);
  return Array.isArray(release?.assets) ? release.assets : [];
}

// An asset the release does not hold (or a platform with no build) lands on the release page.
async function download(request, url, ctx) {
  const [kind, platform] = url.pathname.replace(/^\/download\/?/, '').split('/');
  let prefix = 'tailfuse-app-';
  let suffix;
  if (!kind) suffix = APP_SUFFIX[platformFromUserAgent(request.headers.get('user-agent') ?? '')];
  else if (kind === 'app') suffix = APP_SUFFIX[platform];
  else if (kind === 'cli') {
    prefix = 'tailfuse-v';
    suffix = CLI_SUFFIX[platform];
  }
  if (!suffix) return Response.redirect(RELEASES, 302);
  const assets = await latestAssets(ctx).catch(() => []);
  const asset = assets.find((a) => typeof a.name === 'string' && a.name.startsWith(prefix) && a.name.endsWith(suffix));
  if (!asset?.browser_download_url) return Response.redirect(RELEASES, 302);
  // Stream the file from GitHub's CDN through tailfuse.com, so the visitor stays on this site and
  // the browser saves the file at once. GitHub still serves the bytes; the Worker only relays them.
  const upstream = await fetch(asset.browser_download_url, { headers: { 'user-agent': 'tailfuse-site' }, redirect: 'follow' });
  if (!upstream.ok || !upstream.body) return Response.redirect(asset.browser_download_url, 302);
  const headers = new Headers({
    'content-type': upstream.headers.get('content-type') || 'application/octet-stream',
    'content-disposition': `attachment; filename="${asset.name}"`,
    'cache-control': 'public, max-age=300',
    'x-content-type-options': 'nosniff',
  });
  const length = upstream.headers.get('content-length');
  if (length) headers.set('content-length', length);
  return new Response(upstream.body, { status: 200, headers });
}

// Security headers for every response the site sends.
const SECURITY_HEADERS = {
  'strict-transport-security': 'max-age=31536000; includeSubDomains; preload',
  'x-content-type-options': 'nosniff',
  'x-frame-options': 'DENY',
  'referrer-policy': 'strict-origin-when-cross-origin',
  'permissions-policy': 'camera=(), microphone=(), geolocation=(), payment=(), usb=()',
  'cross-origin-opener-policy': 'same-origin',
  'content-security-policy': [
    "default-src 'self'",
    "script-src 'self'",
    "style-src 'self' https://fonts.googleapis.com",
    "font-src https://fonts.gstatic.com",
    "img-src 'self' data:",
    "connect-src 'self'",
    "form-action 'self'",
    "frame-ancestors 'none'",
    "base-uri 'self'",
    'upgrade-insecure-requests',
  ].join('; '),
};

function secured(response) {
  const r = new Response(response.body, response);
  for (const [k, v] of Object.entries(SECURITY_HEADERS)) if (!r.headers.has(k)) r.headers.set(k, v);
  return r;
}

export default {
  async fetch(request, env, ctx) {
    return secured(await handle(request, env, ctx));
  },
};

async function handle(request, env, ctx) {
  {
    const url = new URL(request.url);

    if (url.hostname === 'www.tailfuse.com') {
      url.hostname = 'tailfuse.com';
      return Response.redirect(url.toString(), 301);
    }

    if (url.pathname === '/download' || url.pathname.startsWith('/download/')) return download(request, url, ctx);

    if (url.pathname.startsWith('/api/')) {
      try {
        if (url.pathname === '/api/status' && request.method === 'GET') {
          return json({ licenses: Boolean(env.LICENSE_PRIVKEY), donations: Boolean(env.LICENSE_PRIVKEY && env.STRIPE_SECRET_KEY) });
        }
        if (url.pathname === '/api/stripe-webhook' && request.method === 'POST') return await stripeWebhook(request, env);
        if (url.pathname === '/api/license' && request.method === 'POST') return await requestLicense(request, env, siteOrigin(env, url));
        if (url.pathname === '/api/license' && request.method === 'GET') return await claimLicense(url, env);
        return json({ error: 'Not found.' }, 404);
      } catch (err) {
        console.error('api error', err?.message ?? err);
        return json({ error: 'Something went wrong issuing the key.' }, 500);
      }
    }

    return env.ASSETS.fetch(request);
  }
}

