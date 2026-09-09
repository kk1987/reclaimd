const BASE = '/api/v1';

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

export async function requestScan(key) {
  return post(`/disks/${encodeURIComponent(key)}/scan`, {});
}

async function post(path, body) {
  const r = await fetch(BASE + path, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) {
    const e = new Error(j?.error?.message || r.statusText);
    e.code = j?.error?.code;
    throw e;
  }
  return j;
}
