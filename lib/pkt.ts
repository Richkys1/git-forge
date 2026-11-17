// pkt-line framing (git smart HTTP) and sideband (band 1 = data, 2 = progress, 3 = error).
// A pkt-line is 4 hex digits of total length (payload + 4) + payload. 0000 = flush,
// 0001 = delim, 0002 = response-end. Max packet 65520 bytes.

export const MAX_PKT = 65520;

/** Encode one payload as a pkt-line: one allocation (header + body in a single buffer). */
export function pkt(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = body.length + 4;
  if (len > MAX_PKT) throw new Error(`pkt-line too large: ${len}`);
  const out = Buffer.alloc(len);
  out.write(len.toString(16).padStart(4, '0'), 0, 'ascii');
  body.copy(out, 4);
  return out;
}

/** A flush packet. */
export const FLUSH = Buffer.from('0000', 'ascii');
/** A delim packet (0001). */
export const DELIM = Buffer.from('0001', 'ascii');
/** A response-end packet (0002). */
export const RESP_END = Buffer.from('0002', 'ascii');

/** A sideband packet for band 1/2/3. Payloads over 65516 bytes are split. */
export function sideband(band: 1 | 2 | 3, data: Buffer): Buffer[] {
  const out: Buffer[] = [];
  let offset = 0;
  do {
    const chunk = data.subarray(offset, offset + MAX_PKT - 5);
    const body = Buffer.concat([Buffer.from([band]), chunk]);
    out.push(pkt(body));
    offset += chunk.length;
  } while (offset < data.length);
  return out;
}

/** Value of one hex digit, or -1 when the byte is not a hex digit. */
function hexNibble(byte: number): number {
  if (byte >= 48 && byte <= 57) return byte - 48; // 0-9
  if (byte >= 97 && byte <= 102) return byte - 97 + 10; // a-f
  if (byte >= 65 && byte <= 70) return byte - 65 + 10; // A-F
  return -1;
}

/**
 * Read a 4-hex-digit pkt-line length at `offset`, or -1 when those bytes are not
 * four hex digits. Replaces a per-frame toString + regex + parseInt with four
 * byte reads; the frame parsers below call it on every pkt-line.
 */
function pktLineLength(buf: Buffer, offset: number): number {
  const a = hexNibble(buf[offset]);
  const b = hexNibble(buf[offset + 1]);
  const c = hexNibble(buf[offset + 2]);
  const d = hexNibble(buf[offset + 3]);
  if (a < 0 || b < 0 || c < 0 || d < 0) return -1;
  return (a << 12) | (b << 8) | (c << 4) | d;
}

/** Parse a byte stream into pkt-line frames. Each frame: {type:'data', data} | {type:'flush'|'delim'|'respEnd'}. */
export interface PktFrame {
  type: 'data' | 'flush' | 'delim' | 'respEnd';
  data: Buffer;
}

export function parsePktLines(buf: Buffer): PktFrame[] {
  const frames: PktFrame[] = [];
  let offset = 0;
  while (offset + 4 <= buf.length) {
    const len = pktLineLength(buf, offset);
    if (len < 0) break;
    if (len === 0) {
      frames.push({ type: 'flush', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (len === 1) {
      frames.push({ type: 'delim', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (len === 2) {
      frames.push({ type: 'respEnd', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (len < 4 || offset + len > buf.length) break;
    frames.push({ type: 'data', data: buf.subarray(offset + 4, offset + len) });
    offset += len;
  }
  return frames;
}

/** Split a byte buffer into the frames after a leading pack of raw bytes (used for push requests). */
export function splitFramesAndRest(buf: Buffer): { frames: PktFrame[]; restOffset: number } {
  const frames: PktFrame[] = [];
  let offset = 0;
  while (offset + 4 <= buf.length) {
    const len = pktLineLength(buf, offset);
    if (len < 0) break;
    if (len === 0) {
      frames.push({ type: 'flush', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (len === 1) {
      frames.push({ type: 'delim', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (len === 2) {
      frames.push({ type: 'respEnd', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (len < 4 || offset + len > buf.length) break;
    frames.push({ type: 'data', data: buf.subarray(offset + 4, offset + len) });
    offset += len;
  }
  return { frames, restOffset: offset };
}

/** Parse a capability line "key" or "key=value" (space separated after the first \0 on a ref line). */
export function parseCapabilities(text: string): Map<string, string> {
  const caps = new Map<string, string>();
  for (const token of text.split(' ')) {
    if (token === '') continue;
    const eq = token.indexOf('=');
    if (eq === -1) caps.set(token, '');
    else caps.set(token.slice(0, eq), token.slice(eq + 1));
  }
  return caps;
}
