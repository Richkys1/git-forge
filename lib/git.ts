// Git object model: SHA-1 content addressing, zlib storage, tree/commit/tag
// build + parse, and packfile read/write (v2, full objects + REF_DELTA/OFS_DELTA).
// Standard git formats only; no git binary anywhere.
import { deflateSync, inflateSync } from 'node:zlib';
import { createHash } from 'node:crypto';

export type ObjType = 'blob' | 'tree' | 'commit' | 'tag';

export interface GitObject {
  type: ObjType;
  data: Buffer;
  /** Content id: sha1("<type> <len>\0<data>"). Always set by storeObject/readPack. */
  oid: string;
}

export const ZERO_OID = '0000000000000000000000000000000000000000';
const SHA1_RE = /^[0-9a-f]{40}$/;

export function isOid(s: string): boolean {
  return SHA1_RE.test(s);
}

/** The git object id: sha1("<type> <len>\0<data>"). */
export function hashObject(type: ObjType, data: Buffer): string {
  const head = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return createHash('sha1').update(head).update(data).digest('hex');
}

export function storeObject(objects: Map<string, GitObject>, type: ObjType, data: Buffer): string {
  const oid = hashObject(type, data);
  if (!objects.has(oid)) objects.set(oid, { type, data, oid });
  return oid;
}

// ---------------------------------------------------------------------------------------------
// trees
// ---------------------------------------------------------------------------------------------

export interface TreeEntry {
  mode: string; // '100644' | '100755' | '120000' | '40000'
  name: string;
  oid: string;
}

/** Tree bytes: entries sorted by name with directories sorted as if named "name/". */
export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const an = a.mode === '40000' ? `${a.name}/` : a.name;
    const bn = b.mode === '40000' ? `${b.name}/` : b.name;
    return an < bn ? -1 : an > bn ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let i = 0;
  while (i < data.length) {
    const sp = data.indexOf(0x20, i);
    const nul = data.indexOf(0x00, sp);
    const mode = data.subarray(i, sp).toString('utf8');
    const name = data.subarray(sp + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    i = nul + 21;
  }
  return entries;
}

/** Look a path up in a tree, walking subtrees. Returns the blob oid + mode, or null. */
export function treeLookup(
  objects: Map<string, GitObject>,
  treeOid: string,
  path: string,
): { oid: string; mode: string } | null {
  const segments = path.split('/').filter((s) => s !== '');
  let cur = treeOid;
  for (let i = 0; i < segments.length; i++) {
    const obj = objects.get(cur);
    if (!obj || obj.type !== 'tree') return null;
    const entry = parseTree(obj.data).find((e) => e.name === segments[i]);
    if (!entry) return null;
    if (i === segments.length - 1) return { oid: entry.oid, mode: entry.mode };
    if (entry.mode !== '40000') return null;
    cur = entry.oid;
  }
  return null;
}

/** Every entry of a tree, flattened to full paths (files only, plus empty dirs are dropped). */
export function treeFlatten(
  objects: Map<string, GitObject>,
  treeOid: string,
  prefix = '',
): { path: string; oid: string; mode: string }[] {
  const obj = objects.get(treeOid);
  if (!obj || obj.type !== 'tree') return [];
  const out: { path: string; oid: string; mode: string }[] = [];
  for (const e of parseTree(obj.data)) {
    const p = prefix === '' ? e.name : `${prefix}/${e.name}`;
    if (e.mode === '40000') out.push(...treeFlatten(objects, e.oid, p));
    else out.push({ path: p, oid: e.oid, mode: e.mode });
  }
  return out;
}

/** A new tree equal to `base` with `path` set to (`oid`, `mode`) — or removed when oid is null. */
export function treeSet(
  objects: Map<string, GitObject>,
  baseTreeOid: string | null,
  path: string,
  oid: string | null,
  mode = '100644',
): string {
  const segments = path.split('/').filter((s) => s !== '');
  const build = (treeOid: string | null, depth: number): string | null => {
    const entries = treeOid === null ? [] : parseTree(objects.get(treeOid)?.data ?? Buffer.alloc(0));
    const name = segments[depth];
    const last = depth === segments.length - 1;
    const idx = entries.findIndex((e) => e.name === name);
    if (last) {
      if (oid === null) {
        if (idx !== -1) entries.splice(idx, 1);
      } else if (idx !== -1) {
        entries[idx] = { mode, name, oid };
      } else {
        entries.push({ mode, name, oid });
      }
    } else {
      const child = idx !== -1 && entries[idx].mode === '40000' ? entries[idx].oid : null;
      const next = build(child, depth + 1);
      if (next === null) {
        if (idx !== -1) entries.splice(idx, 1);
      } else if (idx !== -1) {
        entries[idx] = { mode: '40000', name, oid: next };
      } else {
        entries.push({ mode: '40000', name, oid: next });
      }
    }
    if (entries.length === 0) return null;
    return storeObject(objects, 'tree', buildTree(entries));
  };
  return build(baseTreeOid, 0) ?? storeObject(objects, 'tree', buildTree([]));
}

// ---------------------------------------------------------------------------------------------
// commits and tags
// ---------------------------------------------------------------------------------------------

export interface CommitInfo {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
  authorTs: number;
  committerTs: number;
  authorTz: string;
  committerTz: string;
  /** The author timestamp (alias of authorTs) — the commit's time. */
  timestamp: number;
  authorName: string;
  authorEmail: string;
  committerName: string;
  committerEmail: string;
  gpgsig?: string;
}

/** Split "Name <email>" into its parts. */
function splitIdentity(who: string): { name: string; email: string } {
  const m = who.match(/^(.*?) <([^<>]*)>$/);
  if (m) return { name: m[1].trim(), email: m[2] };
  return { name: who.trim(), email: '' };
}

export function buildCommit(c: {
  tree: string;
  parents: readonly string[];
  message: string;
  author: string;
  committer: string;
  timestamp: number;
  tz?: string;
}): Buffer {
  const tz = c.tz ?? '+0000';
  const lines: string[] = [`tree ${c.tree}`];
  for (const p of c.parents) lines.push(`parent ${p}`);
  lines.push(`author ${c.author} ${c.timestamp} ${tz}`);
  lines.push(`committer ${c.committer} ${c.timestamp} ${tz}`);
  lines.push('');
  lines.push(c.message.endsWith('\n') ? c.message : `${c.message}\n`);
  return Buffer.from(lines.join('\n'), 'utf8');
}

export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const headEnd = text.indexOf('\n\n');
  const head = headEnd === -1 ? text : text.slice(0, headEnd);
  const message = headEnd === -1 ? '' : text.slice(headEnd + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  let authorTs = 0;
  let committerTs = 0;
  let authorTz = '+0000';
  let committerTz = '+0000';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) {
      const m = line.match(/^(.*) (\d+) ([+-]\d{4})$/);
      if (m) {
        author = m[1];
        authorTs = Number(m[2]);
        authorTz = m[3];
      } else author = line.slice(7);
    } else if (line.startsWith('committer ')) {
      const m = line.match(/^(.*) (\d+) ([+-]\d{4})$/);
      if (m) {
        committer = m[1];
        committerTs = Number(m[2]);
        committerTz = m[3];
      } else committer = line.slice(10);
    }
  }
  const authorId = splitIdentity(author);
  const committerId = splitIdentity(committer);
  return {
    tree,
    parents,
    author,
    committer,
    message,
    authorTs,
    committerTs,
    authorTz,
    committerTz,
    timestamp: authorTs,
    authorName: authorId.name,
    authorEmail: authorId.email,
    committerName: committerId.name,
    committerEmail: committerId.email,
  };
}

export function buildTag(t: {
  object: string;
  type: ObjType;
  tag: string;
  message: string;
  tagger: string;
  timestamp: number;
  tz?: string;
}): Buffer {
  const tz = t.tz ?? '+0000';
  const lines = [
    `object ${t.object}`,
    `type ${t.type}`,
    `tag ${t.tag}`,
    `tagger ${t.tagger} ${t.timestamp} ${tz}`,
    '',
    t.message.endsWith('\n') ? t.message : `${t.message}\n`,
  ];
  return Buffer.from(lines.join('\n'), 'utf8');
}

export function parseTag(data: Buffer): { object: string; type: string; tag: string; tagger: string; message: string } {
  const text = data.toString('utf8');
  const headEnd = text.indexOf('\n\n');
  const head = headEnd === -1 ? text : text.slice(0, headEnd);
  const message = headEnd === -1 ? '' : text.slice(headEnd + 2);
  let object = '';
  let type = '';
  let tag = '';
  let tagger = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
    else if (line.startsWith('tagger ')) tagger = line.slice(7).trim();
  }
  return { object, type, tag, tagger, message };
}

// ---------------------------------------------------------------------------------------------
// reachability
// ---------------------------------------------------------------------------------------------

/** All object ids reachable from `tips` (commits walk parents + trees + tag targets). */
export function collectReachable(
  tips: Iterable<string>,
  lookup: (oid: string) => GitObject | undefined,
): Set<string> {
  const seen = new Set<string>();
  const stack = [...tips];
  while (stack.length) {
    const oid = stack.pop()!;
    if (seen.has(oid)) continue;
    const obj = lookup(oid);
    if (!obj) throw new Error(`missing object ${oid}`);
    seen.add(oid);
    if (obj.type === 'commit') {
      const c = parseCommit(obj.data);
      stack.push(c.tree, ...c.parents);
    } else if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    } else if (obj.type === 'tree') {
      for (const e of parseTree(obj.data)) stack.push(e.oid);
    }
  }
  return seen;
}

/** True when `ancestor` is reachable from `tip` (or equal): the fast-forward test. */
export function isAncestor(
  objects: Map<string, GitObject>,
  ancestor: string,
  tip: string,
): boolean {
  if (ancestor === tip) return true;
  const seen = new Set<string>([tip]);
  const stack = [tip];
  while (stack.length) {
    const oid = stack.pop()!;
    const obj = objects.get(oid);
    if (!obj) continue;
    if (obj.type === 'commit') {
      for (const p of parseCommit(obj.data).parents) {
        if (p === ancestor) return true;
        if (!seen.has(p)) {
          seen.add(p);
          stack.push(p);
        }
      }
    } else if (obj.type === 'tag') {
      const t = parseTag(obj.data).object;
      if (t === ancestor) return true;
      if (!seen.has(t)) {
        seen.add(t);
        stack.push(t);
      }
    }
  }
  return false;
}

// ---------------------------------------------------------------------------------------------
// packfiles
// ---------------------------------------------------------------------------------------------

const PACK_TYPES: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };
const TYPE_CODES: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };

/**
 * Read a pack v2/v3 stream. Returns objects in pack order, deltas fully resolved.
 * `lookup` resolves REF_DELTA bases that live in the repository (not the pack); OFS_DELTA
 * bases are always earlier entries in the same pack, so they resolve inline.
 */
export function readPack(buf: Buffer, lookup?: (oid: string) => GitObject | undefined): GitObject[] {
  if (buf.length < 12 || buf.subarray(0, 4).toString('ascii') !== 'PACK') {
    throw new Error('not a packfile');
  }
  const version = buf.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = buf.readUInt32BE(8);
  // Verify the trailing checksum over everything before it.
  const want = createHash('sha1').update(buf.subarray(0, buf.length - 20)).digest('hex');
  const got = buf.subarray(buf.length - 20).toString('hex');
  if (want !== got) throw new Error('pack checksum mismatch');
  let off = 12;
  const objects: GitObject[] = [];
  const byOffset = new Map<number, GitObject>();
  const byOid = new Map<string, GitObject>();
  for (let n = 0; n < count; n++) {
    const start = off;
    let byte = buf[off++];
    const typeCode = (byte >> 4) & 0x7;
    let size = byte & 0x0f;
    let shift = 4;
    while (byte & 0x80) {
      byte = buf[off++];
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    let obj: GitObject;
    if (typeCode === 6) {
      // OFS_DELTA: negative offset to the base object (always an earlier pack entry).
      let b = buf[off++];
      let baseOff = b & 0x7f;
      while (b & 0x80) {
        b = buf[off++];
        baseOff = ((baseOff + 1) << 7) | (b & 0x7f);
      }
      const base = byOffset.get(start - baseOff);
      if (!base) throw new Error('delta base not in pack');
      const len = deflateLen(buf, off);
      const delta = inflateSync(buf.subarray(off, off + len));
      off += len;
      const data = applyDelta(base.data, delta);
      obj = { type: base.type, data, oid: hashObject(base.type, data) };
    } else if (typeCode === 7) {
      // REF_DELTA: base by object id — an earlier pack entry, or in the repository.
      const baseOid = buf.subarray(off, off + 20).toString('hex');
      off += 20;
      const len = deflateLen(buf, off);
      const delta = inflateSync(buf.subarray(off, off + len));
      off += len;
      const base = byOid.get(baseOid) ?? lookup?.(baseOid);
      if (!base) throw new Error(`delta base ${baseOid} not found`);
      const data = applyDelta(base.data, delta);
      obj = { type: base.type, data, oid: hashObject(base.type, data) };
    } else {
      const type = PACK_TYPES[typeCode];
      if (!type) throw new Error(`unknown pack object type ${typeCode}`);
      const len = deflateLen(buf, off);
      const raw = inflateSync(buf.subarray(off, off + len));
      off += len;
      if (raw.length !== size) throw new Error('pack object size mismatch');
      obj = { type, data: raw, oid: hashObject(type, raw) };
    }
    objects.push(obj);
    byOffset.set(start, obj);
    byOid.set(obj.oid, obj);
  }
  return objects;
}

/** Adler-32 checksum, the trailer of every zlib stream. */
function adler32(data: Buffer): number {
  let a = 1;
  let b = 0;
  for (let i = 0; i < data.length; i++) {
    a = (a + data[i]) % 65521;
    b = (b + a) % 65521;
  }
  return ((b << 16) | a) >>> 0;
}

/**
 * Bytes consumed by the zlib stream starting at `off`. inflateSync ignores trailing bytes,
 * so one inflate yields the data; the stream's Adler-32 trailer then locates its end, and a
 * verification inflate on the candidate slice rules out a coincidental match inside the
 * deflate data. Two inflates per object instead of ~2*log2 probes.
 */
function deflateLen(buf: Buffer, off: number): number {
  const data = inflateSync(buf.subarray(off));
  const want = adler32(data);
  let pos = off + 2;
  const max = buf.length - 4;
  while (pos <= max) {
    if (buf.readUInt32BE(pos) === want) {
      const end = pos + 4;
      try {
        if (inflateSync(buf.subarray(off, end)).equals(data)) return end - off;
      } catch {
        // A truncated stream that happens to end in the checksum bytes: keep scanning.
      }
    }
    pos += 1;
  }
  throw new Error('could not find zlib stream end');
}

/** Apply a git delta stream to a base buffer. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let p = 0;
  const readVarint = (): number => {
    let v = 0;
    let shift = 0;
    while (true) {
      const b = delta[p++];
      v |= (b & 0x7f) << shift;
      shift += 7;
      if (!(b & 0x80)) return v;
    }
  };
  const srcSize = readVarint();
  const dstSize = readVarint();
  if (srcSize !== base.length) throw new Error('delta base size mismatch');
  const out: Buffer[] = [];
  let total = 0;
  while (p < delta.length) {
    const op = delta[p++];
    if (op & 0x80) {
      let off = 0;
      let size = 0;
      if (op & 0x01) off = delta[p++];
      if (op & 0x02) off |= delta[p++] << 8;
      if (op & 0x04) off |= delta[p++] << 16;
      if (op & 0x08) off |= delta[p++] << 24;
      if (op & 0x10) size = delta[p++];
      if (op & 0x20) size |= delta[p++] << 8;
      if (op & 0x40) size |= delta[p++] << 16;
      if (size === 0) size = 0x10000;
      out.push(base.subarray(off, off + size));
      total += size;
    } else if (op !== 0) {
      out.push(delta.subarray(p, p + op));
      p += op;
      total += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  if (total !== dstSize) throw new Error('delta result size mismatch');
  return Buffer.concat(out);
}

/** Write a pack v2 of full (undeltified) objects, with trailing checksum. */
export function writePack(objects: readonly GitObject[]): Buffer {
  const parts: Buffer[] = [Buffer.from('PACK', 'ascii')];
  const head = Buffer.alloc(8);
  head.writeUInt32BE(2, 0);
  head.writeUInt32BE(objects.length, 4);
  parts.push(head);
  for (const obj of objects) {
    const code = TYPE_CODES[obj.type];
    const data = obj.data;
    const size = data.length;
    const header: number[] = [];
    let s = size;
    header.push((code << 4) | (s & 0x0f));
    s >>= 4;
    while (s > 0) {
      header[header.length - 1] |= 0x80;
      header.push(s & 0x7f);
      s >>= 7;
    }
    parts.push(Buffer.from(header));
    parts.push(deflateSync(data));
  }
  const body = Buffer.concat(parts);
  const sum = createHash('sha1').update(body).digest();
  return Buffer.concat([body, sum]);
}
