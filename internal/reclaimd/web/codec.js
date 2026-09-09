/* Mirrors the Go encoder in latency.go. The wire format is the storage format:
   a 64-byte header then one little-endian uint16 per block. */

export const LAT_UNIT_US = 32;
export const LAT_SKIPPED = 0xFFFF;
export const LAT_ERROR = 0xFFFE;
export const LAT_MAX = 0xFFFD;

export function decodeProfile(buf) {
  const dv = new DataView(buf);
  const magic = String.fromCharCode(dv.getUint8(0), dv.getUint8(1), dv.getUint8(2), dv.getUint8(3));
  if (magic !== 'RCLM') throw new Error('bad profile magic ' + magic);
  const shift = dv.getUint8(6);
  const count = dv.getUint32(8, true);
  const seq = Number(dv.getBigUint64(12, true));
  const started = Number(dv.getBigUint64(20, true));
  const baselineUs = dv.getUint32(28, true);
  const values = new Uint16Array(buf, 64, count);
  return {
    blockSize: 1 << shift, count, seq, startedTs: started,
    baselineMs: baselineUs / 1000, values,
  };
}

export function codeToMs(code) {
  if (code === LAT_SKIPPED || code === LAT_ERROR) return null;
  return (code * LAT_UNIT_US) / 1000;
}

/* Four buckets keyed to the drive's own learned baseline, so the colours mean
   the same thing on a fast stick and a slow one. */
export function bucketOf(code, baseMs) {
  if (code === LAT_SKIPPED) return 'skip';
  if (code === LAT_ERROR) return 'drop';
  const ms = codeToMs(code);
  const b = baseMs > 0 ? baseMs : 10;
  if (ms >= b * 50) return 3;
  if (ms >= b * 20) return 2;
  if (ms >= b * 5) return 1;
  return 0;
}
