// git smart HTTP: info/refs advertisement (v2 upload-pack, v0 upload/receive-pack),
// v2 ls-refs + fetch, v0 fetch, and receive-pack with report-status.
// Wire formats verified against sdk/conformance/git-client.ts.
import {
  collectReachable,
  isAncestor,
  isOid,
  parseCommit,
  parseTag,
  parseTree,
  readPack,
  writePack,
  ZERO_OID,
  type GitObject,
} from './git.ts';
import { FLUSH, pkt, sideband, splitFramesAndRest } from './pkt.ts';
import { listRefs, type Repo, type Store } from './store.ts';

const AGENT = 'forge/1.0';

/**
 * The v2 upload-pack advertisement, built once at module load: it never depends on
 * the repository (the same six pkt-lines for every request), so there is no reason
 * to allocate six buffers and concatenate them on every info/refs call. The buffer
 * is never mutated after construction (sendBuffer only reads it), so sharing one
 * instance across responses is safe.
 */
const V2_UPLOAD_ADVERTISEMENT = Buffer.concat([
  pkt('version 2\n'),
  pkt(`agent=${AGENT}\n`),
  pkt('ls-refs=unborn\n'),
  pkt('fetch=shallow wait-for-done filter\n'),
  pkt('object-format=sha1\n'),
  FLUSH,
]);

export interface GitResponse {
  status: number;
  contentType: string;
  body: Buffer;
}

function ok(contentType: string, body: Buffer): GitResponse {
  return { status: 200, contentType, body };
}

function notFound(): GitResponse {
  return { status: 404, contentType: 'text/plain', body: Buffer.from('repository not found\n') };
}

function badRequest(message: string): GitResponse {
  return { status: 400, contentType: 'text/plain', body: Buffer.from(`${message}\n`) };
}

/** The first ref line (or the capabilities^{} line) carries these capabilities. */
const V0_UPLOAD_CAPS =
  'multi_ack_detailed side-band-64k thin-pack ofs-delta shallow filter agent=forge/1.0 symref=HEAD:refs/heads/main';
const V0_RECEIVE_CAPS =
  'report-status side-band-64k delete-refs ofs-delta agent=forge/1.0 symref=HEAD:refs/heads/main';

/** Peeled commit for an annotated tag ref, else null. */
function peeledOf(repo: Repo, refName: string, oid: string): string | null {
  const obj = repo.objects.get(oid);
  if (!obj || obj.type !== 'tag') return null;
  const tag = parseTag(obj.data);
  return tag.type === 'commit' ? tag.object : null;
}

function headTarget(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

/** v0 ref lines: first line carries \0caps; annotated tags get a ^{} peeled line after. */
function v0RefLines(repo: Repo, caps: string): Buffer[] {
  const refs = listRefs(repo);
  const lines: Buffer[] = [];
  if (refs.length === 0) {
    lines.push(pkt(`${ZERO_OID} capabilities^{}\0${caps}\n`));
    return lines;
  }
  refs.forEach((ref, index) => {
    const first = index === 0;
    lines.push(pkt(`${ref.oid} ${ref.name}${first ? `\0${caps}` : ''}\n`));
    const peeled = peeledOf(repo, ref.name, ref.oid);
    if (peeled) lines.push(pkt(`${peeled} ${ref.name}^{}\n`));
  });
  return lines;
}

// -------------------------------------------------------------------------------------------
// info/refs
// -------------------------------------------------------------------------------------------

export function handleInfoRefs(
  store: Store,
  owner: string,
  repoName: string,
  service: string,
  gitProtocol: string | undefined,
): GitResponse {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return notFound();
  const v2 = gitProtocol === 'version=2';
  if (service === 'git-upload-pack' && v2) {
    return ok('application/x-git-upload-pack-advertisement', V2_UPLOAD_ADVERTISEMENT);
  }
  if (service === 'git-upload-pack') {
    const parts: Buffer[] = [pkt('# service=git-upload-pack\n'), FLUSH, ...v0RefLines(repo, V0_UPLOAD_CAPS), FLUSH];
    return ok('application/x-git-upload-pack-advertisement', Buffer.concat(parts));
  }
  if (service === 'git-receive-pack') {
    const parts: Buffer[] = [pkt('# service=git-receive-pack\n'), FLUSH, ...v0RefLines(repo, V0_RECEIVE_CAPS), FLUSH];
    return ok('application/x-git-receive-pack-advertisement', Buffer.concat(parts));
  }
  return notFound();
}

// -------------------------------------------------------------------------------------------
// fetch object computation
// -------------------------------------------------------------------------------------------

export interface FetchRequest {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  deepenSince: number | null;
  deepenNot: string[];
}

interface FetchPlan {
  objects: GitObject[];
  shallow: string[];
  unshallow: string[];
  wantedRefs: { oid: string; ref: string }[];
}

/**
 * Walk from the wants, honouring depth / deepen-since / deepen-not, skipping anything
 * the client already holds. The held set is reachable from the haves WITHOUT crossing a
 * shallow boundary: a shallow commit's parents are not held, so a deepen can deliver them.
 */
function planFetch(repo: Repo, req: FetchRequest): FetchPlan {
  const shallowSet = new Set(req.shallow);
  const heldSet = new Set<string>();
  const addTreeToSet = (set: Set<string>, treeOid: string): void => {
    const stack = [treeOid];
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (set.has(oid)) continue;
      const obj = repo.objects.get(oid);
      if (!obj) continue;
      set.add(oid);
      if (obj.type === 'tree') {
        for (const entry of parseTree(obj.data)) stack.push(entry.oid);
      }
    }
  };
  // Held = reachable from the haves, stopping at shallow boundaries.
  {
    const queue = [...req.haves];
    const seen = new Set<string>();
    while (queue.length > 0) {
      const oid = queue.shift()!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const obj = repo.objects.get(oid);
      if (!obj) continue;
      if (obj.type === 'tag') {
        heldSet.add(oid);
        const tag = parseTag(obj.data);
        if (!seen.has(tag.object)) queue.push(tag.object);
        continue;
      }
      if (obj.type !== 'commit') {
        heldSet.add(oid);
        continue;
      }
      heldSet.add(oid);
      const info = parseCommit(obj.data);
      addTreeToSet(heldSet, info.tree);
      if (shallowSet.has(oid)) continue; // boundary: its parents are not held
      for (const parent of info.parents) if (!seen.has(parent)) queue.push(parent);
    }
  }
  const excluded = new Set<string>();
  for (const ref of req.deepenNot) {
    const tip = repo.refs.get(ref);
    if (tip) for (const oid of collectReachable([tip], (o) => repo.objects.get(o))) excluded.add(oid);
  }
  const sentCommits = new Set<string>();
  const objects = new Map<string, GitObject>();
  /** Add a commit's tree closure only (tree + blobs + subtrees) — never its ancestors,
   * and never anything the client already holds. */
  const addTreeClosure = (treeOid: string): void => {
    const stack = [treeOid];
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (objects.has(oid) || heldSet.has(oid) || excluded.has(oid)) continue;
      const obj = repo.objects.get(oid);
      if (!obj) continue;
      objects.set(oid, obj);
      if (obj.type === 'tree') {
        for (const entry of parseTree(obj.data)) stack.push(entry.oid);
      }
    }
  };
  // BFS over commits, level = distance from the nearest want.
  const queue: { oid: string; level: number }[] = req.wants.map((oid) => ({ oid, level: 0 }));
  const seen = new Set<string>();
  while (queue.length > 0) {
    const { oid, level } = queue.shift()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = repo.objects.get(oid);
    if (!obj) continue;
    if (obj.type === 'tag') {
      if (!heldSet.has(oid) && !excluded.has(oid)) objects.set(oid, obj);
      const tag = parseTag(obj.data);
      if (!seen.has(tag.object)) queue.push({ oid: tag.object, level });
      continue;
    }
    if (obj.type !== 'commit') {
      if (!heldSet.has(oid) && !excluded.has(oid)) objects.set(oid, obj);
      continue;
    }
    if (excluded.has(oid)) continue;
    const info = parseCommit(obj.data);
    // deepen-since: an older commit is a boundary — neither it nor its ancestors are sent.
    if (req.deepenSince !== null && info.timestamp < req.deepenSince) continue;
    const withinDepth = req.depth === null || level < req.depth;
    if (withinDepth && !heldSet.has(oid)) {
      sentCommits.add(oid);
      objects.set(oid, obj);
      addTreeClosure(info.tree);
    }
    // Parents are worth visiting only while they could still be within the depth.
    if (req.depth === null || level + 1 < req.depth) {
      for (const parent of info.parents) {
        if (!seen.has(parent)) queue.push({ oid: parent, level: level + 1 });
      }
    }
  }
  // Boundary: a sent commit whose parent exists but is neither sent nor held.
  const shallow: string[] = [];
  for (const oid of sentCommits) {
    const obj = repo.objects.get(oid);
    if (!obj) continue;
    const info = parseCommit(obj.data);
    for (const parent of info.parents) {
      if (!sentCommits.has(parent) && !heldSet.has(parent) && repo.objects.has(parent)) {
        shallow.push(oid);
        break;
      }
    }
  }
  // Unshallow: a requested boundary that is sent and no longer a boundary.
  const shallowBoundary = new Set(shallow);
  const unshallow: string[] = [];
  for (const oid of req.shallow) {
    if (sentCommits.has(oid) && !shallowBoundary.has(oid)) unshallow.push(oid);
  }
  const wantSet = new Set(req.wants);
  const wantedRefs = listRefs(repo)
    .filter((ref) => wantSet.has(ref.oid))
    .map((ref) => ({ oid: ref.oid, ref: ref.name }));
  // Deterministic pack order: sort by oid.
  const sorted = [...objects.values()].sort((a, b) => (a.oid < b.oid ? -1 : a.oid > b.oid ? 1 : 0));
  return { objects: sorted, shallow, unshallow, wantedRefs };
}

function ackLines(repo: Repo, req: FetchRequest): string[] {
  const lines: string[] = [];
  let any = false;
  for (const have of req.haves) {
    if (repo.objects.has(have)) {
      lines.push(`ACK ${have}\n`);
      any = true;
    }
  }
  if (any) lines.push('ready\n');
  else lines.push('NAK\n');
  return lines;
}

// -------------------------------------------------------------------------------------------
// upload-pack (ls-refs / fetch, v2 and v0)
// -------------------------------------------------------------------------------------------

export function handleUploadPack(
  store: Store,
  owner: string,
  repoName: string,
  body: Buffer,
  gitProtocol: string | undefined,
): GitResponse {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return notFound();
  try {
    const { frames } = splitFramesAndRest(body);
    // Data lines plus delim markers (delim frames are not data frames).
    const items: (string | 'delim')[] = frames
      .filter((f) => f.type === 'data' || f.type === 'delim')
      .map((f) => (f.type === 'delim' ? 'delim' : f.data.toString('utf8')));
    const lines = items.filter((i): i is string => i !== 'delim');
    const command = lines[0]?.replace(/\n$/, '') ?? '';
    if (command.startsWith('command=ls-refs')) return handleLsRefs(repo, items);
    if (command.startsWith('command=fetch')) return handleFetchV2(repo, items);
    // No command= line: a v0 fetch (want/have/done) or garbage.
    if (lines.some((l) => l.startsWith('want '))) return handleFetchV0(repo, frames, body);
    return badRequest('unknown upload-pack request');
  } catch (err) {
    return badRequest(`upload-pack error: ${err instanceof Error ? err.message : String(err)}`);
  }
}

function handleLsRefs(repo: Repo, items: (string | 'delim')[]): GitResponse {
  // After the delim: peel / symrefs / unborn / ref-prefix <p> arguments.
  let peel = false;
  let symrefs = false;
  let unborn = false;
  const prefixes: string[] = [];
  let pastDelim = false;
  for (const item of items) {
    if (item === 'delim') {
      pastDelim = true;
      continue;
    }
    const text = item.replace(/\n$/, '');
    if (text === 'command=ls-refs' || text.startsWith('agent=') || text.startsWith('object-format=')) continue;
    if (!pastDelim) continue;
    if (text === 'peel') peel = true;
    else if (text === 'symrefs') symrefs = true;
    else if (text === 'unborn') unborn = true;
    else if (text.startsWith('ref-prefix ')) prefixes.push(text.slice('ref-prefix '.length));
  }
  const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
  const out: Buffer[] = [];
  const headRef = headTarget(repo);
  const headTip = repo.refs.get(headRef) ?? null;
  if (symrefs && keep('HEAD')) {
    if (headTip) out.push(pkt(`${headTip} HEAD symref-target:${headRef}\n`));
    else if (unborn) out.push(pkt(`unborn HEAD symref-target:${headRef}\n`));
  }
  for (const ref of listRefs(repo)) {
    if (!keep(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (peel) {
      const peeled = peeledOf(repo, ref.name, ref.oid);
      if (peeled) line += ` peeled:${peeled}`;
    }
    out.push(pkt(`${line}\n`));
  }
  out.push(FLUSH);
  return ok('application/x-git-upload-pack-result', Buffer.concat(out));
}

function parseFetchArgs(items: (string | 'delim')[]): FetchRequest {
  const req: FetchRequest = { wants: [], haves: [], shallow: [], depth: null, deepenSince: null, deepenNot: [] };
  let pastDelim = false;
  for (const item of items) {
    if (item === 'delim') {
      pastDelim = true;
      continue;
    }
    const text = item.replace(/\n$/, '');
    if (text === 'command=fetch' || text.startsWith('agent=') || text.startsWith('object-format=')) continue;
    if (!pastDelim) continue;
    if (text === 'thin-pack' || text === 'no-progress' || text === 'ofs-delta' || text === 'done') continue;
    if (text.startsWith('want ')) req.wants.push(text.slice(5).trim());
    else if (text.startsWith('have ')) req.haves.push(text.slice(5).trim());
    else if (text.startsWith('shallow ')) req.shallow.push(text.slice(8).trim());
    else if (text.startsWith('deepen ')) req.depth = Number(text.slice(7));
    else if (text.startsWith('deepen-since ')) req.deepenSince = Number(text.slice(13));
    else if (text.startsWith('deepen-not ')) req.deepenNot.push(text.slice(11).trim());
  }
  return req;
}

function handleFetchV2(repo: Repo, items: (string | 'delim')[]): GitResponse {
  const req = parseFetchArgs(items);
  const plan = planFetch(repo, req);
  const pack = writePack(plan.objects);
  const parts: Buffer[] = [];
  parts.push(pkt('acknowledgments\n'));
  for (const line of ackLines(repo, req)) parts.push(pkt(line));
  parts.push(FLUSH);
  parts.push(pkt('shallow-info\n'));
  for (const oid of plan.shallow) parts.push(pkt(`shallow ${oid}\n`));
  for (const oid of plan.unshallow) parts.push(pkt(`unshallow ${oid}\n`));
  parts.push(FLUSH);
  parts.push(pkt('wanted-refs\n'));
  for (const ref of plan.wantedRefs) parts.push(pkt(`${ref.oid} ${ref.ref}\n`));
  parts.push(FLUSH);
  parts.push(pkt('packfile\n'));
  for (const chunk of sideband(1, pack)) parts.push(chunk);
  parts.push(FLUSH);
  return ok('application/x-git-upload-pack-result', Buffer.concat(parts));
}

function handleFetchV0(repo: Repo, frames: { type: string; data: Buffer }[], body: Buffer): GitResponse {
  // v0: want lines (first carries caps after a space), shallow/deepen lines, flush,
  // then have/done lines, flush. Parse all data frames in order.
  const req: FetchRequest = { wants: [], haves: [], shallow: [], depth: null, deepenSince: null, deepenNot: [] };
  for (const frame of frames) {
    if (frame.type !== 'data') continue;
    const text = frame.data.toString('utf8').replace(/\n$/, '');
    if (text.startsWith('want ')) {
      const rest = text.slice(5);
      const oid = rest.split(' ')[0];
      if (isOid(oid)) req.wants.push(oid);
    } else if (text.startsWith('have ')) req.haves.push(text.slice(5).trim());
    else if (text.startsWith('shallow ')) req.shallow.push(text.slice(8).trim());
    else if (text.startsWith('deepen ')) req.depth = Number(text.slice(7));
    else if (text.startsWith('deepen-since ')) req.deepenSince = Number(text.slice(13));
    else if (text.startsWith('deepen-not ')) req.deepenNot.push(text.slice(11).trim());
  }
  const plan = planFetch(repo, req);
  const pack = writePack(plan.objects);
  const parts: Buffer[] = [];
  for (const line of ackLines(repo, req)) parts.push(pkt(line));
  for (const oid of plan.shallow) parts.push(pkt(`shallow ${oid}\n`));
  for (const oid of plan.unshallow) parts.push(pkt(`unshallow ${oid}\n`));
  for (const chunk of sideband(1, pack)) parts.push(chunk);
  parts.push(FLUSH);
  return ok('application/x-git-upload-pack-result', Buffer.concat(parts));
}

// -------------------------------------------------------------------------------------------
// receive-pack
// -------------------------------------------------------------------------------------------

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

export function handleReceivePack(store: Store, owner: string, repoName: string, body: Buffer): GitResponse {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return notFound();
  try {
    const { frames, restOffset } = splitFramesAndRest(body);
    const commands: PushCommand[] = [];
    for (const frame of frames) {
      if (frame.type !== 'data') continue;
      const text = frame.data.toString('utf8').replace(/\n$/, '');
      const nul = text.indexOf('\0');
      const head = nul >= 0 ? text.slice(0, nul) : text;
      const parts = head.split(' ');
      if (parts.length < 3 || !isOid(parts[0]) || !isOid(parts[1])) continue;
      commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts[2] });
    }
    if (commands.length === 0) return badRequest('no push commands');
    const hasUpdate = commands.some((c) => c.newOid !== ZERO_OID);
    let unpackOk = true;
    let unpackDetail = 'ok';
    if (hasUpdate) {
      const packBytes = body.subarray(restOffset);
      if (packBytes.length > 0) {
        try {
          const objects = readPack(packBytes);
          for (const obj of objects) repo.objects.set(obj.oid, obj);
        } catch (err) {
          unpackOk = false;
          unpackDetail = `unpack error: ${err instanceof Error ? err.message : String(err)}`;
        }
      }
    }
    const report: string[] = [];
    report.push(`unpack ${unpackOk ? 'ok' : unpackDetail}\n`);
    for (const cmd of commands) {
      let result: { ok: true } | { ok: false; reason: string };
      if (!unpackOk) {
        result = { ok: false, reason: 'unpack failed' };
      } else {
        result = applyPushCommand(repo, cmd);
      }
      if (result.ok) report.push(`ok ${cmd.ref}\n`);
      else report.push(`ng ${cmd.ref} ${result.reason}\n`);
    }
    const band: Buffer[] = [];
    for (const line of report) band.push(pkt(line));
    const parts: Buffer[] = [...sideband(1, Buffer.concat(band)), FLUSH];
    return ok('application/x-git-receive-pack-result', Buffer.concat(parts));
  } catch (err) {
    return badRequest(`receive-pack error: ${err instanceof Error ? err.message : String(err)}`);
  }
}

function applyPushCommand(repo: Repo, cmd: PushCommand): { ok: true } | { ok: false; reason: string } {
  const current = repo.refs.get(cmd.ref) ?? null;
  if (cmd.newOid === ZERO_OID) {
    // Delete.
    if (current === null) return { ok: false, reason: 'non-fast-forward' };
    if (cmd.oldOid !== ZERO_OID && cmd.oldOid !== current) return { ok: false, reason: 'non-fast-forward' };
    repo.refs.delete(cmd.ref);
    return { ok: true };
  }
  if (cmd.oldOid === ZERO_OID) {
    // Create.
    if (current !== null) return { ok: false, reason: 'non-fast-forward' };
    if (!repo.objects.has(cmd.newOid)) return { ok: false, reason: 'non-fast-forward' };
    repo.refs.set(cmd.ref, cmd.newOid);
    return { ok: true };
  }
  // Update: old must match the tip and the new tip must descend from it.
  if (current === null) return { ok: false, reason: 'non-fast-forward' };
  if (current !== cmd.oldOid) return { ok: false, reason: 'non-fast-forward' };
  if (cmd.newOid === cmd.oldOid) return { ok: true };
  if (!repo.objects.has(cmd.newOid)) return { ok: false, reason: 'non-fast-forward' };
  if (!isAncestor(repo.objects, cmd.oldOid, cmd.newOid)) return { ok: false, reason: 'non-fast-forward' };
  repo.refs.set(cmd.ref, cmd.newOid);
  return { ok: true };
}
