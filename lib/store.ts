// In-memory store for the forge. Everything is deterministic: ids and numbers come
// from counters, timestamps from a fixed epoch plus a per-event counter, so two runs
// of the same trial leave the same state. No wall clock, no Math.random.
//
// The scenario the suite drives is fixed (wire.ts buildScenario): the viewer is always
// adaiversen (id 1000); the stranger is olgabauer. Auth is a single bearer token from
// MAKEFUN_SHOP_TOKEN — any request carrying it is the viewer; without it, 401.
import {
  buildCommit,
  buildTree,
  collectReachable,
  hashObject,
  isAncestor,
  parseCommit,
  parseTree,
  storeObject,
  treeFlatten,
  treeLookup,
  treeSet,
  ZERO_OID,
  type GitObject,
  type ObjType,
} from './git.ts';

export const VIEWER = { id: 1000, login: 'adaiversen', name: 'ada iversen', email: 'adaiversen@example.invalid' };
export const STRANGER = { id: 1002, login: 'olgabauer', name: 'olga bauer', email: 'olgabauer@example.invalid' };
export const SCENARIO_EPOCH_SEC = 1735689600; // 2025-01-01T00:00:00Z

export interface Label {
  name: string;
  color: string;
  description: string;
}

export interface IssueComment {
  id: number;
  body: string;
  user: string;
  createdAt: string;
  updatedAt: string;
}

export interface Issue {
  id: string; // opaque global id
  databaseId: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  user: string;
  createdAt: string;
  updatedAt: string;
  comments: IssueComment[];
}

export interface PullFile {
  filename: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  changes: number;
  patch?: string;
}

export interface Pull {
  id: string;
  databaseId: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: { ref: string; sha: string; repo: string };
  base: { ref: string; sha: string; repo: string };
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  mergeable: 'MERGEABLE' | 'CONFLICTING' | 'UNKNOWN';
  user: string;
  createdAt: string;
  updatedAt: string;
  files: PullFile[];
  /** The head tree's files at creation, for conflict detection on merge. */
  headTree: string;
  baseTree: string;
  /** Paths the head branch changed vs base (for files + merge). */
  changedPaths: string[];
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdAt: string;
  updatedAt: string;
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  issues: Map<number, Issue>;
  nextIssue: number;
  nextIssueId: number;
  labels: Map<string, Label>;
  pulls: Map<number, Pull>;
  nextPull: number;
  nextPullId: number;
  nextCommentId: number;
  /** Blob ids for the read-workload fast path are just the issues map; nothing extra. */
}

/** All refs of a repo, sorted by name. */
export function listRefs(repo: Repo): { name: string; oid: string }[] {
  const out: { name: string; oid: string }[] = [];
  for (const name of [...repo.refs.keys()].sort()) out.push({ name, oid: repo.refs.get(name)! });
  return out;
}
export function globalId(kind: string, ...parts: (string | number)[]): string {
  return Buffer.from(`${kind}:${parts.join(':')}`, 'utf8').toString('base64url');
}

export function decodeGlobalId(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const idx = text.indexOf(':');
    if (idx === -1) return null;
    return { kind: text.slice(0, idx), parts: text.slice(idx + 1).split(':') };
  } catch {
    return null;
  }
}

export class Store {
  readonly repos = new Map<string, Repo>(); // key `${owner}/${name}`
  private nextRepoId = 5000;
  private nextGlobalSeq = 1;
  /** Deterministic clock: seconds since the scenario epoch. */
  private tick = 0;

  /** A timestamp string, one second later on every call — deterministic across runs. */
  nowIso(): string {
    const epoch = SCENARIO_EPOCH_SEC + this.tick;
    this.tick += 1;
    return new Date(epoch * 1000).toISOString().replace(/\.\d{3}Z$/, '.000Z');
  }

  /** A commit timestamp (seconds), one second later on every call. */
  nowSec(): number {
    const epoch = SCENARIO_EPOCH_SEC + this.tick;
    this.tick += 1;
    return epoch;
  }

  /** The trial's fixed now when the harness sets it, else the scenario epoch. */
  static trialEpochSec(): number {
    const raw = process.env.MAKEFUN_TRIAL_NOW ?? '';
    const ms = Number(raw);
    if (Number.isFinite(ms) && ms > 0) return Math.floor(ms / 1000);
    const iso = Date.parse(raw);
    if (Number.isFinite(iso)) return Math.floor(iso / 1000);
    return SCENARIO_EPOCH_SEC;
  }

  key(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  listRepos(owner: string): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner === owner) out.push(repo);
    out.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
    return out;
  }

  createRepo(owner: string, name: string, options: { description?: string; private?: boolean; defaultBranch?: string }): Repo | null {
    if (this.repos.has(this.key(owner, name))) return null;
    const now = this.nowIso();
    const repo: Repo = {
      id: this.nextRepoId++,
      name,
      owner,
      description: options.description ?? '',
      private: options.private ?? false,
      defaultBranch: options.defaultBranch ?? 'main',
      createdAt: now,
      updatedAt: now,
      objects: new Map(),
      refs: new Map(),
      issues: new Map(),
      nextIssue: 1,
      nextIssueId: 1,
      labels: new Map(),
      pulls: new Map(),
      nextPull: 1,
      nextPullId: 1,
      nextCommentId: 1,
    };
    this.repos.set(this.key(owner, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.key(owner, name));
  }

  // -------------------------------------------------------------------------------------------
  // git helpers on a repo
  // -------------------------------------------------------------------------------------------

  /** The tip of a ref, or null when the ref does not exist. */
  refTip(repo: Repo, ref: string): string | null {
    return repo.refs.get(ref) ?? null;
  }

  /** Set a ref (fast-forward rules are enforced by the caller). */
  setRef(repo: Repo, ref: string, oid: string): void {
    repo.refs.set(ref, oid);
  }

  deleteRef(repo: Repo, ref: string): boolean {
    return repo.refs.delete(ref);
  }

  /** The default branch's tip, or null when the repo is unborn. */
  headTip(repo: Repo): string | null {
    return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
  }

  /** The tree oid of a commit, or null. */
  commitTree(repo: Repo, commitOid: string): string | null {
    const obj = repo.objects.get(commitOid);
    if (!obj || obj.type !== 'commit') return null;
    return parseCommit(obj.data).tree;
  }

  /** All refs sorted by name, with HEAD first when asked. */
  listRefs(repo: Repo): { name: string; oid: string }[] {
    return listRefs(repo);
  }

  /** Objects reachable from the given tips that the caller does not already have. */
  missingObjects(repo: Repo, tips: readonly string[], have: ReadonlySet<string>): GitObject[] {
    const reachable = collectReachable(tips, (oid) => repo.objects.get(oid));
    const out: GitObject[] = [];
    for (const oid of reachable) {
      if (have.has(oid)) continue;
      const obj = repo.objects.get(oid);
      if (obj) out.push(obj);
    }
    return out;
  }

  /**
   * Create a commit on a branch from a tree mutation: set `path` to `content` (or delete when
   * content is null), parented on the branch tip (or none when unborn). Returns the commit oid.
   */
  commitFile(
    repo: Repo,
    branch: string,
    path: string,
    content: Buffer | null,
    message: string,
    author: string,
  ): { commit: string; blob: string | null; tree: string } {
    const ref = `refs/heads/${branch}`;
    const tip = repo.refs.get(ref) ?? null;
    const baseTree = tip === null ? null : this.commitTree(repo, tip);
    let blob: string | null = null;
    let tree: string;
    if (content === null) {
      tree = treeSet(repo.objects, baseTree, path, null);
    } else {
      blob = storeObject(repo.objects, 'blob', content);
      tree = treeSet(repo.objects, baseTree, path, blob, '100644');
    }
    const ts = this.nowSec();
    const commitData = buildCommit({
      tree,
      parents: tip === null ? [] : [tip],
      message,
      author,
      committer: author,
      timestamp: ts,
    });
    const commit = storeObject(repo.objects, 'commit', commitData);
    repo.refs.set(ref, commit);
    return { commit, blob, tree };
  }

  /** The file at `path` on a ref's tip: {blob oid, content} or null. */
  readFile(repo: Repo, ref: string, path: string): { oid: string; mode: string; data: Buffer } | null {
    const tip = repo.refs.get(ref);
    if (!tip) return null;
    const tree = this.commitTree(repo, tip);
    if (!tree) return null;
    const found = treeLookup(repo.objects, tree, path);
    if (!found) return null;
    const obj = repo.objects.get(found.oid);
    if (!obj || obj.type !== 'blob') return null;
    return { oid: found.oid, mode: found.mode, data: obj.data };
  }

  /** The flat file list of a ref's tip tree. */
  listFiles(repo: Repo, ref: string): { path: string; oid: string; mode: string }[] {
    const tip = repo.refs.get(ref);
    if (!tip) return [];
    const tree = this.commitTree(repo, tip);
    if (!tree) return [];
    return treeFlatten(repo.objects, tree);
  }

  /** Diff two trees: paths added/modified/removed with per-file line counts. */
  diffTrees(repo: Repo, baseTree: string, headTree: string): PullFile[] {
    const base = new Map(treeFlatten(repo.objects, baseTree).map((f) => [f.path, f]));
    const head = new Map(treeFlatten(repo.objects, headTree).map((f) => [f.path, f]));
    const files: PullFile[] = [];
    for (const [path, h] of head) {
      const b = base.get(path);
      if (!b) {
        const text = repo.objects.get(h.oid)?.data.toString('utf8') ?? '';
        files.push({ filename: path, status: 'added', additions: countLines(text), deletions: 0, changes: countLines(text) });
      } else if (b.oid !== h.oid) {
        const oldText = repo.objects.get(b.oid)?.data.toString('utf8') ?? '';
        const newText = repo.objects.get(h.oid)?.data.toString('utf8') ?? '';
        const add = countLines(newText);
        const del = countLines(oldText);
        files.push({ filename: path, status: 'modified', additions: add, deletions: del, changes: add + del });
      }
    }
    for (const [path, b] of base) {
      if (!head.has(path)) {
        const text = repo.objects.get(b.oid)?.data.toString('utf8') ?? '';
        files.push({ filename: path, status: 'removed', additions: 0, deletions: countLines(text), changes: countLines(text) });
      }
    }
    files.sort((a, b) => (a.filename < b.filename ? -1 : 1));
    return files;
  }

  /** True when the head branch can merge cleanly onto base (no same-file conflict). */
  mergeable(repo: Repo, pull: Pull): boolean {
    const baseFiles = new Map(treeFlatten(repo.objects, pull.baseTree).map((f) => [f.path, f.oid]));
    const headFiles = new Map(treeFlatten(repo.objects, pull.headTree).map((f) => [f.path, f.oid]));
    const baseTip = repo.refs.get(`refs/heads/${pull.base.ref}`);
    if (!baseTip) return false;
    const baseTreeNow = this.commitTree(repo, baseTip);
    if (!baseTreeNow) return false;
    const nowFiles = new Map(treeFlatten(repo.objects, baseTreeNow).map((f) => [f.path, f.oid]));
    for (const [path, headOid] of headFiles) {
      const baseOid = baseFiles.get(path);
      if (baseOid === undefined) {
        // Added on head: conflicts only if base also added it differently.
        const nowOid = nowFiles.get(path);
        if (nowOid !== undefined && nowOid !== headOid) return false;
      } else if (headOid !== baseOid) {
        const nowOid = nowFiles.get(path);
        if (nowOid !== undefined && nowOid !== baseOid) return false;
      }
    }
    for (const [path] of baseFiles) {
      if (!headFiles.has(path)) {
        const nowOid = nowFiles.get(path);
        if (nowOid !== undefined && nowOid !== baseFiles.get(path)) return false;
      }
    }
    return true;
  }

  /**
   * Squash-merge a pull: one commit on the base branch whose tree is the head tree merged
   * over the base tree (head wins per path), parented on the base tip. Returns the commit oid.
   */
  squashMerge(repo: Repo, pull: Pull, message: string, author: string): string {
    const baseTip = repo.refs.get(`refs/heads/${pull.base.ref}`)!;
    const baseTree = this.commitTree(repo, baseTip)!;
    // Apply every head-vs-base diff onto the base tree.
    const files = this.diffTrees(repo, pull.baseTree, pull.headTree);
    let tree = baseTree;
    for (const f of files) {
      if (f.status === 'removed') {
        tree = treeSet(repo.objects, tree, f.filename, null);
      } else {
        const headFile = treeLookup(repo.objects, pull.headTree, f.filename);
        if (headFile) tree = treeSet(repo.objects, tree, f.filename, headFile.oid, headFile.mode);
      }
    }
    const ts = this.nowSec();
    const commit = storeObject(
      repo.objects,
      'commit',
      buildCommit({ tree, parents: [baseTip], message, author, committer: author, timestamp: ts }),
    );
    repo.refs.set(`refs/heads/${pull.base.ref}`, commit);
    return commit;
  }

  /** Re-derive a pull's head sha/tree after its branch moved. */
  refreshPullHead(repo: Repo, pull: Pull): void {
    const tip = repo.refs.get(`refs/heads/${pull.head.ref}`);
    if (!tip) return;
    pull.head.sha = tip;
    const tree = this.commitTree(repo, tip);
    if (tree) {
      pull.headTree = tree;
      pull.files = this.diffTrees(repo, pull.baseTree, tree);
      pull.changedPaths = pull.files.map((f) => f.filename);
    }
    pull.mergeable = this.mergeable(repo, pull) ? 'MERGEABLE' : 'CONFLICTING';
  }

  // -------------------------------------------------------------------------------------------
  // issues / pulls
  // -------------------------------------------------------------------------------------------

  createIssue(repo: Repo, title: string, body: string, labels: string[], user: string): Issue {
    const number = repo.nextIssue++;
    const db = repo.nextIssueId++;
    const now = this.nowIso();
    const issue: Issue = {
      id: globalId('Issue', repo.id, number),
      databaseId: db,
      number,
      title,
      body,
      state: 'open',
      labels: [...labels],
      user,
      createdAt: now,
      updatedAt: now,
      comments: [],
    };
    repo.issues.set(number, issue);
    return issue;
  }

  createPull(
    repo: Repo,
    input: { title: string; body: string; head: string; base: string; user: string },
  ): Pull | null {
    const headTip = repo.refs.get(`refs/heads/${input.head}`);
    const baseTip = repo.refs.get(`refs/heads/${input.base}`);
    if (!headTip || !baseTip) return null;
    const headTree = this.commitTree(repo, headTip)!;
    const baseTree = this.commitTree(repo, baseTip)!;
    const files = this.diffTrees(repo, baseTree, headTree);
    const number = repo.nextPull++;
    const db = repo.nextPullId++;
    const now = this.nowIso();
    const pull: Pull = {
      id: globalId('PullRequest', repo.id, number),
      databaseId: db,
      number,
      title: input.title,
      body: input.body,
      state: 'open',
      head: { ref: input.head, sha: headTip, repo: repo.name },
      base: { ref: input.base, sha: baseTip, repo: repo.name },
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      mergeable: 'UNKNOWN',
      user: input.user,
      createdAt: now,
      updatedAt: now,
      files,
      headTree,
      baseTree,
      changedPaths: files.map((f) => f.filename),
    };
    pull.mergeable = this.mergeable(repo, pull) ? 'MERGEABLE' : 'CONFLICTING';
    repo.pulls.set(number, pull);
    return pull;
  }
}

export function countLines(text: string): number {
  if (text === '') return 0;
  return text.split('\n').length - (text.endsWith('\n') ? 1 : 0);
}

export { ZERO_OID, isAncestor, hashObject, parseCommit, parseTree, storeObject, collectReachable };
export type { GitObject, ObjType };
