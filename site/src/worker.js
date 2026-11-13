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
  });
  if (!session.ok || !session.body.url) {
    console.error('stripe checkout create failed', session.status, session.body?.error?.message);
    return json({ error: 'The payment page could not be opened. Try again, or pick $0.' }, 502);
  }
  return json({ url: session.body.url });
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
  const key = await signLicense(env.LICENSE_PRIVKEY, email, new Date(session.body.created * 1000));
  return json({ key, email });
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
  return Response.redirect(asset?.browser_download_url ?? RELEASES, 302);
}

export default {
  async fetch(request, env, ctx) {
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
        if (url.pathname === '/api/license' && request.method === 'POST') return await requestLicense(request, env, url.origin);
        if (url.pathname === '/api/license' && request.method === 'GET') return await claimLicense(url, env);
        return json({ error: 'Not found.' }, 404);
      } catch (err) {
        console.error('api error', err?.message ?? err);
        return json({ error: 'Something went wrong issuing the key.' }, 500);
      }
    }

    return env.ASSETS.fetch(request);
  },
};
