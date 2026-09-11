/* SSE with three gates, because a naive stream is worse than polling here:
   the scanner can emit a hundred events a second and the page may sit open,
   unattended, on a router for weeks. */

import { BASE } from './api.js';

const HIDE_GRACE_MS = 60_000;

export function connect(handlers) {
  let es = null;
  let hideTimer = null;
  let failures = 0;
  let pollTimer = null;
  let closed = false;

  function open() {
    if (closed || es) return;
    try {
      es = new EventSource(BASE + '/stream');
    } catch (e) {
      startPolling();
      return;
    }
    es.addEventListener('open', () => { failures = 0; handlers.onStatus?.('live'); });
    for (const name of ['hello', 'progress', 'event', 'state', 'resync']) {
      es.addEventListener(name, (ev) => {
        let data = {};
        try { data = JSON.parse(ev.data); } catch (e) { return; }
        handlers['on' + name[0].toUpperCase() + name.slice(1)]?.(data);
      });
    }
    es.addEventListener('error', () => {
      handlers.onStatus?.('down');
      close();
      failures++;
      /* Three consecutive failures means EventSource is not going to work here
         (an old browser, a proxy that buffers), so fall back instead of
         retrying forever in silence. */
      if (failures >= 3) startPolling();
      else if (!closed) setTimeout(open, Math.min(1000 * failures, 5000));
    });
  }

  function close() {
    if (es) { es.close(); es = null; }
  }

  /* The gate that matters most for an unattended tab: after a minute hidden,
     drop the connection entirely. It costs the router nothing to have nobody
     watching, and the handler below reopens the stream the moment the tab is
     visible again. */
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') {
      hideTimer = setTimeout(() => { close(); handlers.onStatus?.('idle'); }, HIDE_GRACE_MS);
    } else {
      clearTimeout(hideTimer);
      open();
    }
  });

  function startPolling() {
    if (pollTimer || closed) return;
    const tick = async () => {
      if (closed) return;
      try {
        const r = await fetch(BASE + '/disks', { credentials: 'same-origin' });
        if (r.ok) {
          handlers.onState?.({ change: 'POLL', refetch: ['disk'] });
          handlers.onStatus?.('live');
        }
      } catch (e) { handlers.onStatus?.('down'); }
      const visible = document.visibilityState === 'visible';
      pollTimer = setTimeout(tick, visible ? 5000 : 60000);
    };
    tick();
  }

  open();
  return { close() { closed = true; close(); clearTimeout(pollTimer); } };
}
