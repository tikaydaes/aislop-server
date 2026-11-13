'use strict';
// Passkey sign-in, registration and the contest form. No dependencies.
(() => {
  const $ = (sel) => document.querySelector(sel);
  const say = (text) => { const m = $('#msg'); if (m) m.textContent = text; };

  async function api(path, body, raw) {
    const res = await fetch(path, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: raw ? body : JSON.stringify(body || {}),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.message || data.error || `Request failed (${res.status})`);
    return data;
  }

  // ---- proof of work: find a nonce so sha256(challenge:nonce) starts with N zero bits
  const K = new Uint32Array([
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3, 0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2]);
  const W = new Uint32Array(64);

  // First 32 bits of SHA-256 of an ASCII string shorter than 56 bytes.
  function sha256Head(str) {
    const block = new Uint8Array(64);
    for (let i = 0; i < str.length; i++) block[i] = str.charCodeAt(i);
    block[str.length] = 0x80;
    const bits = str.length * 8;
    block[62] = bits >>> 8; block[63] = bits & 255;
    for (let i = 0; i < 16; i++) W[i] = (block[i * 4] << 24) | (block[i * 4 + 1] << 16) | (block[i * 4 + 2] << 8) | block[i * 4 + 3];
    for (let i = 16; i < 64; i++) {
      const a = W[i - 15], b = W[i - 2];
      const s0 = ((a >>> 7) | (a << 25)) ^ ((a >>> 18) | (a << 14)) ^ (a >>> 3);
      const s1 = ((b >>> 17) | (b << 15)) ^ ((b >>> 19) | (b << 13)) ^ (b >>> 10);
      W[i] = (W[i - 16] + s0 + W[i - 7] + s1) | 0;
    }
    let a = 0x6a09e667, b = 0xbb67ae85, c = 0x3c6ef372, d = 0xa54ff53a, e = 0x510e527f, f = 0x9b05688c, g = 0x1f83d9ab, h = 0x5be0cd19;
    for (let i = 0; i < 64; i++) {
      const S1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
      const t1 = (h + S1 + ((e & f) ^ (~e & g)) + K[i] + W[i]) | 0;
      const S0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
      const t2 = (S0 + ((a & b) ^ (a & c) ^ (b & c))) | 0;
      h = g; g = f; f = e; e = (d + t1) | 0; d = c; c = b; b = a; a = (t1 + t2) | 0;
    }
    return (0x6a09e667 + a) >>> 0;
  }

  async function solvePow() {
    const res = await fetch('/auth/pow');
    if (!res.ok) throw new Error('Could not start; try again in a while.');
    const { challenge, bits } = await res.json();
    if (bits > 32) throw new Error('Unsupported challenge.');
    const limit = bits === 0 ? 0xffffffff : (2 ** (32 - bits)) - 1;
    for (let n = 0; ; n++) {
      if (sha256Head(`${challenge}:${n}`) <= limit) return { challenge, nonce: String(n) };
      if (n % 20000 === 19999) await new Promise((r) => setTimeout(r)); // keep the page responsive
    }
  }

  // ---- passkeys
  function needPasskeys() {
    if (!window.PublicKeyCredential || !PublicKeyCredential.parseCreationOptionsFromJSON) {
      throw new Error('This browser does not support passkeys.');
    }
  }

  async function register(name) {
    needPasskeys();
    say('Preparing…');
    const signedIn = !$('#auth');
    const pow = signedIn ? {} : await solvePow();
    const begin = await api('/auth/register/begin', { name, ...pow });
    say('Follow your browser’s passkey prompt…');
    const cred = await navigator.credentials.create({
      publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(begin.options.publicKey),
    });
    await api(`/auth/register/finish?ceremony=${encodeURIComponent(begin.ceremony)}`, JSON.stringify(cred.toJSON()), true);
  }

  async function login() {
    needPasskeys();
    const begin = await api('/auth/login/begin');
    say('Follow your browser’s passkey prompt…');
    const cred = await navigator.credentials.get({
      publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(begin.options.publicKey),
    });
    await api(`/auth/login/finish?ceremony=${encodeURIComponent(begin.ceremony)}`, JSON.stringify(cred.toJSON()), true);
  }

  function run(button, fn, done) {
    button.addEventListener('click', async () => {
      button.disabled = true;
      try { await fn(); done(); } catch (e) {
        say(e.name === 'NotAllowedError' ? 'Cancelled, or no passkey was offered.' : e.message);
      } finally { button.disabled = false; }
    });
  }

  const auth = $('#auth');
  if (auth) {
    const go = () => { location.href = auth.dataset.next || '/account'; };
    run($('#login'), login, go);
    run($('#register'), () => register($('#name').value), go);
  }
  const add = $('#add-passkey');
  if (add) run(add, () => register(''), () => location.reload());

  const contest = $('#contest');
  if (contest) {
    contest.addEventListener('submit', async (e) => {
      e.preventDefault();
      const button = contest.querySelector('button');
      button.disabled = true;
      try {
        say('Sending…');
        const f = new FormData(contest);
        await api('/contest', { target: f.get('target'), reason: f.get('reason'), contact: f.get('contact'), ...(await solvePow()) });
        contest.reset();
        say('Thank you. A person will review this.');
      } catch (err) { say(err.message); } finally { button.disabled = false; }
    });
  }
})();
