/* Resolved from where this page is actually served, not hardcoded to the
   root: on a router the status page usually ends up behind the nginx that is
   already there, mounted under a prefix like /reclaimd/. An absolute '/api/v1'
   would then be fetched from the proxy's root, which is somebody else's app. */
export const BASE = new URL('api/v1', document.baseURI).pathname;

/* Completed passes are immutable, so their profiles are cached forever both by
   the browser and here. The stacked view then costs one fetch per pass for the
   life of the page instead of one per redraw. */
const profileCache = new Map();

async function get(path, opts) {
  const r = await fetch(BASE + path, Object.assign({ credentials: 'same-origin' }, opts));
  if (!r.ok) throw new Error(path + ' -> ' + r.status);
  return r;
}

export async function getDisks() {
  return (await (await get('/disks')).json()).disks || [];
}

export async function getDisk(key) {
  return (await get('/disks/' + encodeURIComponent(key))).json();
}

export async function getRounds(key, limit = 200) {
  return (await (await get(`/disks/${encodeURIComponent(key)}/rounds?limit=${limit}`)).json()).rounds || [];
}

export async function getEvents(key, limit = 120) {
  return (await (await get(`/disks/${encodeURIComponent(key)}/events?limit=${limit}`)).json()).events || [];
}

export async function getProfile(key, round = 'latest', res = 'full') {
  const id = `${key}|${round}|${res}`;
  if (round !== 'latest' && profileCache.has(id)) return profileCache.get(id);
  const r = await get(`/disks/${encodeURIComponent(key)}/profile?round=${round}&res=${res}`);
  const buf = await r.arrayBuffer();
  if (round !== 'latest') profileCache.set(id, buf);
  return buf;
}

/* Freshness changes every pass, so unlike a completed profile it is never
   cached on either side. */
export async function getFreshness(key) {
  const r = await get(`/disks/${encodeURIComponent(key)}/freshness`);
  return r.arrayBuffer();
}

export async function setEnabled(key, enabled) {
  return post(`/disks/${encodeURIComponent(key)}/enabled`, { enabled });
}

/* iMeanIt clears the cooldown the last round opened. Named after the CLI flag
   it mirrors, and awkward in both places on purpose. */
export async function requestScan(key, iMeanIt = false) {
  return post(`/disks/${encodeURIComponent(key)}/scan`, { i_mean_it: iMeanIt });
}

/* Deletes everything the daemon has stored for one disk. The confirmation is
   the UI's job; by the time this is called the decision has been made. */
export async function forgetDisk(key) {
  return send('DELETE', `/disks/${encodeURIComponent(key)}`);
}

function post(path, body) {
  return send('POST', path, body);
}

async function send(method, path, body) {
  const opts = { method, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers = { 'Content-Type': 'application/json' };
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(BASE + path, opts);
  const j = await r.json().catch(() => ({}));
  if (!r.ok) {
    /* The daemon answers with a code, never a sentence, so the code is what
       the caller branches on and the message is only a fallback. */
    const e = new Error(j?.error?.message || r.statusText);
    e.code = j?.error?.code;
    throw e;
  }
  return j;
}
