// pkt-line framing (git smart HTTP) and sideband (band 1 = data, 2 = progress, 3 = error).
// A pkt-line is 4 hex digits of total length (payload + 4) + payload. 0000 = flush,
// 0001 = delim, 0002 = response-end. Max packet 65520 bytes.

export const MAX_PKT = 65520;

/** Encode one payload as a pkt-line. */
export function pkt(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = body.length + 4;
  if (len > MAX_PKT) throw new Error(`pkt-line too large: ${len}`);
  const head = Buffer.alloc(4);
  head.write(len.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, body]);
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

/** Parse a byte stream into pkt-line frames. Each frame: {type:'data', data} | {type:'flush'|'delim'|'respEnd'}. */
export interface PktFrame {
  type: 'data' | 'flush' | 'delim' | 'respEnd';
  data: Buffer;
}

export function parsePktLines(buf: Buffer): PktFrame[] {
  const frames: PktFrame[] = [];
  let offset = 0;
  while (offset + 4 <= buf.length) {
    const lenHex = buf.toString('ascii', offset, offset + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(lenHex)) break;
    const len = parseInt(lenHex, 16);
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
    const lenHex = buf.toString('ascii', offset, offset + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(lenHex)) break;
    const len = parseInt(lenHex, 16);
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
