// REST v3 (GitHub-compatible subset) under /api/v3.
// Every reply is application/json with a non-empty X-GitHub-Request-Id; errors are
// {"message": string}. Auth: Bearer <MAKEFUN_SHOP_TOKEN> is the viewer; no token is 401;
// a repo under any other owner is 404, never 403.
import { isOid, parseCommit, parseTree, treeFlatten, treeLookup } from './git.ts';
import { listRefs, VIEWER, type Issue, type Pull, type Repo, type Store } from './store.ts';

export interface RestResult {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

export interface RestRequest {
  method: string;
  segments: string[]; // path segments after /api/v3, decoded
  query: URLSearchParams;
  headers: Record<string, string>;
  body: unknown; // parsed JSON, or undefined when no body
  rawBody: string;
  host: string; // for absolute Link URLs
}

const json = (status: number, body: unknown, headers?: Record<string, string>): RestResult => ({ status, body, headers });
const err = (status: number, message: string): RestResult => json(status, { message });
const notFound = (what = 'Not Found'): RestResult => err(404, what);

function repoJson(repo: Repo, host: string): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: `R_${repo.id}`,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: repo.owner === VIEWER.login ? VIEWER.id : 0, type: 'User' },
    private: repo.private,
    description: repo.description,
    fork: false,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    pushed_at: repo.updatedAt,
    homepage: '',
    default_branch: repo.defaultBranch,
    html_url: `http://${host}/${repo.owner}/${repo.name}`,
    clone_url: `http://${host}/${repo.owner}/${repo.name}.git`,
    git_url: `git://${host}/${repo.owner}/${repo.name}.git`,
    ssh_url: `git@${host}:${repo.owner}/${repo.name}.git`,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
  };
}

function issueJson(issue: Issue, repo: Repo): Record<string, unknown> {
  return {
    id: issue.databaseId,
    node_id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    locked: false,
    user: { login: issue.user, id: VIEWER.id, type: 'User' },
    labels: issue.labels.map((name) => ({ name, color: repo.labels.get(name)?.color ?? 'ededed', description: '' })),
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.state === 'closed' ? issue.updatedAt : null,
    comments: issue.comments.length,
  };
}

function pullJson(pull: Pull, repo: Repo): Record<string, unknown> {
  return {
    id: pull.databaseId,
    node_id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    locked: false,
    user: { login: pull.user, id: VIEWER.id, type: 'User' },
    head: { label: `${repo.owner}:${pull.head.ref}`, ref: pull.head.ref, sha: pull.head.sha, repo: { name: repo.name, full_name: `${repo.owner}/${repo.name}` } },
    base: { label: `${repo.owner}:${pull.base.ref}`, ref: pull.base.ref, sha: pull.base.sha, repo: { name: repo.name, full_name: `${repo.owner}/${repo.name}` } },
    merged: pull.merged,
    mergeable: pull.mergeable === 'MERGEABLE',
    mergeable_state: pull.mergeable === 'MERGEABLE' ? 'clean' : pull.mergeable === 'CONFLICTING' ? 'dirty' : 'unknown',
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    closed_at: pull.state === 'closed' ? pull.updatedAt : null,
  };
}

function parsePage(query: URLSearchParams): { perPage: number; page: number } {
  const perPage = Math.max(1, Math.min(100, Number(query.get('per_page') ?? '30') || 30));
  const page = Math.max(1, Number(query.get('page') ?? '1') || 1);
  return { perPage, page };
}

function pageOf<T>(items: T[], query: URLSearchParams): { page: T[]; total: number; perPage: number; pageNum: number } {
  const { perPage, page } = parsePage(query);
  const start = (page - 1) * perPage;
  return { page: items.slice(start, start + perPage), total: items.length, perPage, pageNum: page };
}

function linkHeader(host: string, path: string, query: URLSearchParams, total: number, perPage: number, page: number): string | null {
  const lastPage = Math.max(1, Math.ceil(total / perPage));
  if (lastPage <= 1) return null;
  const url = (p: number): string => {
    const q = new URLSearchParams(query);
    q.set('page', String(p));
    q.set('per_page', String(perPage));
    return `http://${host}${path}?${q.toString()}`;
  };
  const parts: string[] = [];
  if (page < lastPage) parts.push(`<${url(page + 1)}>; rel="next"`);
  if (page > 1) parts.push(`<${url(page - 1)}>; rel="prev"`);
  parts.push(`<${url(1)}>; rel="first"`);
  parts.push(`<${url(lastPage)}>; rel="last"`);
  return parts.join(', ');
}

/** Commits reachable from a tip, newest first (first-parent walk is enough for the suite). */
function commitsFrom(repo: Repo, tip: string | null): { sha: string; commit: Record<string, unknown> }[] {
  if (!tip) return [];
  const out: { sha: string; commit: Record<string, unknown> }[] = [];
  const seen = new Set<string>();
  const queue: string[] = [tip];
  while (queue.length > 0) {
    const oid = queue.shift()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = repo.objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    const info = parseCommit(obj.data);
    const who = { name: info.authorName, email: info.authorEmail, date: new Date(info.timestamp * 1000).toISOString() };
    // GitHub's REST API returns the message without the trailing newline the object carries.
    const message = info.message.endsWith('\n') ? info.message.slice(0, -1) : info.message;
    out.push({
      sha: oid,
      commit: {
        message,
        author: who,
        committer: { name: info.committerName, email: info.committerEmail, date: new Date(info.timestamp * 1000).toISOString() },
        tree: { sha: info.tree },
      },
      author: { login: VIEWER.login },
      committer: { login: VIEWER.login },
      parents: info.parents.map((p) => ({ sha: p })),
    });
    for (const parent of info.parents) if (!seen.has(parent)) queue.push(parent);
    if (out.length >= 1000) break;
  }
  return out;
}

export function handleRest(store: Store, req: RestRequest): RestResult {
  const seg = req.segments;
  const host = req.host;
  const body = (req.body ?? {}) as Record<string, unknown>;

  // GET /api/v3/ (root) and /api/v3 (no slash)
  if (seg.length === 0) {
    if (req.method === 'GET') return json(200, {}, { 'X-OAuth-Scopes': 'repo, user' });
    return notFound('Not Found');
  }

  // GET /api/v3/meta
  if (seg.length === 1 && seg[0] === 'meta') {
    if (req.method !== 'GET') return notFound();
    return json(200, { installed_version: '1.0.0', verifiable_password_authentication: false });
  }

  // GET /api/v3/user
  if (seg.length === 1 && seg[0] === 'user') {
    if (req.method !== 'GET') return notFound();
    return json(200, {
      login: VIEWER.login,
      id: VIEWER.id,
      node_id: `U_${VIEWER.id}`,
      name: VIEWER.name,
      email: VIEWER.email,
      type: 'User',
      avatar_url: '',
      html_url: `http://${host}/${VIEWER.login}`,
    });
  }

  // /api/v3/user/repos
  if (seg.length === 2 && seg[0] === 'user' && seg[1] === 'repos') {
    if (req.method === 'GET') {
      const repos = store.listRepos(VIEWER.login);
      const { page, total, perPage, pageNum } = pageOf(repos, req.query);
      const link = linkHeader(host, '/api/v3/user/repos', req.query, total, perPage, pageNum);
      return json(200, page.map((r) => repoJson(r, host)), link ? { Link: link } : undefined);
    }
    if (req.method === 'POST') {
      const name = typeof body.name === 'string' ? body.name : '';
      if (!name || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/.test(name)) return err(422, 'name is required and must be a valid repository name');
      const existing = store.getRepo(VIEWER.login, name);
      if (existing) return err(422, `Repository ${VIEWER.login}/${name} already exists`);
      const repo = store.createRepo(VIEWER.login, name, {
        description: typeof body.description === 'string' ? body.description : '',
        private: body.private === true,
      })!;
      return json(201, repoJson(repo, host));
    }
    return notFound();
  }

  // /api/v3/repos/{owner}/{repo}[/...]
  if (seg.length >= 3 && seg[0] === 'repos') {
    const owner = seg[1];
    const repoName = seg[2];
    if (owner !== VIEWER.login) return notFound(`Repository ${owner}/${repoName} not found`);
    const repo = store.getRepo(owner, repoName);
    const rest = seg.slice(3);

    // GET/PATCH/DELETE /repos/{owner}/{repo}
    if (rest.length === 0) {
      if (req.method === 'GET') {
        if (!repo) return notFound(`Repository ${owner}/${repoName} not found`);
        return json(200, repoJson(repo, host));
      }
      if (req.method === 'PATCH') {
        if (!repo) return notFound(`Repository ${owner}/${repoName} not found`);
        if (typeof body.description === 'string') repo.description = body.description;
        if (typeof body.private === 'boolean') repo.private = body.private;
        repo.updatedAt = store.nowIso();
        return json(200, repoJson(repo, host));
      }
      if (req.method === 'DELETE') {
        if (!repo) return notFound(`Repository ${owner}/${repoName} not found`);
        store.deleteRepo(owner, repoName);
        return { status: 204, body: null };
      }
      return notFound();
    }
    if (!repo) return notFound(`Repository ${owner}/${repoName} not found`);

    // /branches
    if (rest[0] === 'branches') {
      if (rest.length === 1 && req.method === 'GET') {
        const branches = listRefs(repo)
          .filter((r) => r.name.startsWith('refs/heads/'))
          .map((r) => ({ name: r.name.slice('refs/heads/'.length), commit: { sha: r.oid }, protected: false }));
        branches.sort((a, b) => (a.name < b.name ? -1 : 1));
        return json(200, branches);
      }
      if (rest.length === 2 && req.method === 'GET') {
        const name = rest[1];
        const tip = repo.refs.get(`refs/heads/${name}`);
        if (!tip) return notFound(`Branch ${name} not found`);
        return json(200, { name, commit: { sha: tip }, protected: false });
      }
      return notFound();
    }

    // /commits
    if (rest[0] === 'commits' && rest.length === 1 && req.method === 'GET') {
      const shaParam = req.query.get('sha');
      const tip = shaParam ? repo.refs.get(`refs/heads/${shaParam}`) ?? (isOid(shaParam) ? shaParam : null) : store.headTip(repo);
      const commits = commitsFrom(repo, tip);
      return json(200, commits);
    }

    // /git/refs
    if (rest[0] === 'git' && rest[1] === 'refs') {
      if (req.method === 'POST' && rest.length === 2) {
        const ref = typeof body.ref === 'string' ? body.ref : '';
        const sha = typeof body.sha === 'string' ? body.sha : '';
        if (!ref.startsWith('refs/') || !isOid(sha)) return err(422, 'ref and sha are required');
        if (!repo.objects.has(sha)) return err(422, `object ${sha} does not exist`);
        if (repo.refs.has(ref)) return err(422, `Reference ${ref} already exists`);
        repo.refs.set(ref, sha);
        return json(201, { ref, object: { sha, type: 'commit' } });
      }
      if (req.method === 'GET') {
        const wanted = rest.slice(2).join('/');
        if (wanted === '') {
          return json(
            200,
            listRefs(repo).map((r) => ({ ref: r.name, object: { sha: r.oid, type: 'commit' } })),
          );
        }
        const full = `refs/${wanted}`;
        const tip = repo.refs.get(full);
        if (!tip) return notFound(`Reference ${full} not found`);
        return json(200, { ref: full, object: { sha: tip, type: 'commit' } });
      }
      return notFound();
    }

    // /contents/{path...}
    if (rest[0] === 'contents') {
      const path = rest.slice(1).join('/');
      return handleContents(store, repo, req, path);
    }

    // /labels
    if (rest[0] === 'labels') {
      if (req.method === 'GET' && rest.length === 1) {
        return json(200, [...repo.labels.values()].map((l) => ({ name: l.name, color: l.color, description: l.description })));
      }
      if (req.method === 'POST' && rest.length === 1) {
        const name = typeof body.name === 'string' ? body.name : '';
        if (!name) return err(422, 'name is required');
        const color = typeof body.color === 'string' ? body.color : 'ededed';
        const existing = repo.labels.get(name);
        if (existing) return json(201, { name: existing.name, color: existing.color, description: existing.description });
        const label = { name, color, description: typeof body.description === 'string' ? body.description : '' };
        repo.labels.set(name, label);
        return json(201, label);
      }
      return notFound();
    }

    // /issues[/...]
    if (rest[0] === 'issues') {
      return handleIssues(store, repo, req, rest.slice(1));
    }

    // /pulls[/...]
    if (rest[0] === 'pulls') {
      return handlePulls(store, repo, req, rest.slice(1));
    }

    return notFound('Not Found');
  }

  return notFound('Not Found');
}

function handleContents(store: Store, repo: Repo, req: RestRequest, path: string): RestResult {
  const body = (req.body ?? {}) as Record<string, unknown>;
  const author = `${VIEWER.name} <${VIEWER.email}>`;
  const branch = typeof body.branch === 'string' ? body.branch : typeof req.query.get('branch') === 'string' ? (req.query.get('branch') as string) : repo.defaultBranch;
  const refParam = req.query.get('ref');
  const readRef = refParam ?? branch;

  if (req.method === 'GET') {
    const refName = `refs/heads/${readRef}`;
    const tip = repo.refs.get(refName);
    if (!tip) return notFound(`Branch ${readRef} not found`);
    const tree = store.commitTree(repo, tip);
    if (!tree) return notFound(`File ${path} not found`);
    const found = treeLookup(repo.objects, tree, path);
    if (found) {
      const obj = repo.objects.get(found.oid);
      if (obj && obj.type === 'blob') {
        return json(200, {
          type: 'file',
          encoding: 'base64',
          size: obj.data.length,
          name: path.split('/').pop() ?? path,
          path,
          content: obj.data.toString('base64'),
          sha: found.oid,
        });
      }
      // A tree at this path is a directory: fall through to the listing below.
      if (!obj || obj.type !== 'tree') return notFound(`File ${path} not found`);
    }
    // Directory listing: immediate children of path ('' = root).
    const files = treeFlatten(repo.objects, tree);
    const prefix = path === '' ? '' : `${path}/`;
    const children = new Map<string, { type: 'file' | 'dir'; name: string; path: string; sha: string }>();
    for (const file of files) {
      if (!file.path.startsWith(prefix)) continue;
      const rest = file.path.slice(prefix.length);
      if (rest === '') continue;
      const slash = rest.indexOf('/');
      if (slash === -1) {
        children.set(rest, { type: 'file', name: rest, path: file.path, sha: file.oid });
      } else {
        const dirName = rest.slice(0, slash);
        if (!children.has(dirName)) {
          // The dir's sha is its tree oid: find it via the parent tree.
          children.set(dirName, { type: 'dir', name: dirName, path: `${prefix}${dirName}`, sha: dirTreeSha(repo, tree, `${prefix}${dirName}`) });
        }
      }
    }
    if (children.size === 0) return notFound(`File ${path} not found`);
    return json(200, [...children.values()].sort((a, b) => (a.name < b.name ? -1 : 1)));
  }

  if (req.method === 'PUT') {
    const message = typeof body.message === 'string' ? body.message : 'Update file';
    const contentB64 = typeof body.content === 'string' ? body.content : '';
    if (!contentB64) return err(422, 'content is required');
    const content = Buffer.from(contentB64.replace(/[\r\n\t ]/g, ''), 'base64');
    const sha = typeof body.sha === 'string' ? body.sha : null;
    // Branch: create from the default tip when it does not exist yet.
    const refName = `refs/heads/${branch}`;
    let tip = repo.refs.get(refName) ?? null;
    if (tip === null && branch !== repo.defaultBranch) {
      const base = store.headTip(repo);
      if (base !== null) repo.refs.set(refName, base);
      tip = repo.refs.get(refName) ?? null;
    }
    const existing = tip === null ? null : store.readFile(repo, refName, path);
    const isCreate = existing === null;
    if (!isCreate && sha !== null && sha !== existing!.oid) return err(409, `sha ${sha} does not match the current blob ${existing!.oid}`);
    const { commit, blob } = store.commitFile(repo, branch, path, content, message, author);
    return json(isCreate ? 201 : 200, {
      content: { name: path.split('/').pop() ?? path, path, sha: blob, size: content.length, encoding: 'base64' },
      commit: { sha: commit, message },
    });
  }

  if (req.method === 'DELETE') {
    const message = typeof body.message === 'string' ? body.message : 'Delete file';
    const sha = typeof body.sha === 'string' ? body.sha : null;
    const refName = `refs/heads/${branch}`;
    const existing = store.readFile(repo, refName, path);
    if (!existing) return notFound(`File ${path} not found`);
    if (sha !== null && sha !== existing.oid) return err(409, `sha ${sha} does not match the current blob ${existing.oid}`);
    const { commit } = store.commitFile(repo, branch, path, null, message, author);
    return json(200, { commit: { sha: commit, message } });
  }

  return notFound();
}

function dirTreeSha(repo: Repo, rootTree: string, dirPath: string): string {
  // Walk the tree to the directory and return its tree oid (or the root tree when not found).
  const parts = dirPath.split('/').filter(Boolean);
  let current = rootTree;
  for (const part of parts) {
    const entries = parseTree(repo.objects.get(current)?.data ?? Buffer.alloc(0));
    const next = entries.find((e: { name: string; mode: string }) => e.name === part && e.mode === '40000');
    if (!next) return current;
    current = next.oid;
  }
  return current;
}

function handleIssues(store: Store, repo: Repo, req: RestRequest, rest: string[]): RestResult {
  const body = (req.body ?? {}) as Record<string, unknown>;
  // /issues
  if (rest.length === 0) {
    if (req.method === 'POST') {
      const title = typeof body.title === 'string' ? body.title : '';
      const issueBody = typeof body.body === 'string' ? body.body : '';
      const labels = Array.isArray(body.labels) ? body.labels.filter((l): l is string => typeof l === 'string') : [];
      if (!title) return err(422, 'title is required');
      for (const label of labels) {
        if (!repo.labels.has(label)) return err(422, `Label ${label} does not exist`);
      }
      const issue = store.createIssue(repo, title, issueBody, labels, VIEWER.login);
      return json(201, issueJson(issue, repo));
    }
    if (req.method === 'GET') {
      const state = req.query.get('state') ?? 'open';
      const sort = req.query.get('sort') ?? 'created';
      const direction = req.query.get('direction') ?? 'desc';
      let issues = [...repo.issues.values()];
      if (state === 'open') issues = issues.filter((i) => i.state === 'open');
      else if (state === 'closed') issues = issues.filter((i) => i.state === 'closed');
      if (sort === 'created') {
        issues.sort((a, b) => (a.createdAt < b.createdAt ? -1 : a.createdAt > b.createdAt ? 1 : a.number - b.number));
        if (direction === 'desc') issues.reverse();
      }
      const { page, total, perPage, pageNum } = pageOf(issues, req.query);
      const link = linkHeader(req.host, `/api/v3/repos/${repo.owner}/${repo.name}/issues`, req.query, total, perPage, pageNum);
      return json(200, page.map((i) => issueJson(i, repo)), link ? { Link: link } : undefined);
    }
    return notFound();
  }
  const number = Number(rest[0]);
  if (!Number.isInteger(number) || number < 1) return notFound();
  const issue = repo.issues.get(number);
  if (!issue) return notFound(`Issue #${number} not found`);
  // /issues/{n}
  if (rest.length === 1) {
    if (req.method === 'GET') return json(200, issueJson(issue, repo));
    if (req.method === 'PATCH') {
      if (typeof body.state === 'string' && (body.state === 'open' || body.state === 'closed')) issue.state = body.state;
      if (typeof body.title === 'string') issue.title = body.title;
      if (typeof body.body === 'string') issue.body = body.body;
      issue.updatedAt = store.nowIso();
      return json(200, issueJson(issue, repo));
    }
    return notFound();
  }
  // /issues/{n}/comments
  if (rest.length === 2 && rest[1] === 'comments') {
    if (req.method === 'GET') {
      return json(
        200,
        issue.comments.map((c) => ({ id: c.id, body: c.body, user: { login: c.user }, created_at: c.createdAt, updated_at: c.updatedAt })),
      );
    }
    if (req.method === 'POST') {
      const commentBody = typeof body.body === 'string' ? body.body : '';
      if (!commentBody) return err(422, 'body is required');
      const comment = {
        id: repo.nextCommentId++,
        body: commentBody,
        user: VIEWER.login,
        createdAt: store.nowIso(),
        updatedAt: store.nowIso(),
      };
      issue.comments.push(comment);
      return json(201, { id: comment.id, body: comment.body, user: { login: comment.user }, created_at: comment.createdAt, updated_at: comment.updatedAt });
    }
    return notFound();
  }
  return notFound();
}

function handlePulls(store: Store, repo: Repo, req: RestRequest, rest: string[]): RestResult {
  const body = (req.body ?? {}) as Record<string, unknown>;
  // /pulls
  if (rest.length === 0) {
    if (req.method === 'POST') {
      const title = typeof body.title === 'string' ? body.title : '';
      const pullBody = typeof body.body === 'string' ? body.body : '';
      const head = typeof body.head === 'string' ? body.head : '';
      const base = typeof body.base === 'string' ? body.base : repo.defaultBranch;
      if (!title || !head) return err(422, 'title and head are required');
      if (!repo.refs.has(`refs/heads/${head}`)) return err(422, `head branch ${head} does not exist`);
      if (!repo.refs.has(`refs/heads/${base}`)) return err(422, `base branch ${base} does not exist`);
      const pull = store.createPull(repo, { title, body: pullBody, head, base, user: VIEWER.login });
      if (!pull) return err(422, 'could not create pull request');
      return json(201, pullJson(pull, repo));
    }
    if (req.method === 'GET') {
      const state = req.query.get('state') ?? 'open';
      let pulls = [...repo.pulls.values()];
      if (state === 'open') pulls = pulls.filter((p) => p.state === 'open');
      else if (state === 'closed') pulls = pulls.filter((p) => p.state === 'closed');
      pulls.sort((a, b) => b.number - a.number);
      const { page, total, perPage, pageNum } = pageOf(pulls, req.query);
      const link = linkHeader(req.host, `/api/v3/repos/${repo.owner}/${repo.name}/pulls`, req.query, total, perPage, pageNum);
      return json(200, page.map((p) => pullJson(p, repo)), link ? { Link: link } : undefined);
    }
    return notFound();
  }
  const number = Number(rest[0]);
  if (!Number.isInteger(number) || number < 1) return notFound();
  const pull = repo.pulls.get(number);
  if (!pull) return notFound(`Pull request #${number} not found`);
  // /pulls/{n}
  if (rest.length === 1) {
    if (req.method === 'GET') {
      store.refreshPullHead(repo, pull);
      return json(200, pullJson(pull, repo));
    }
    return notFound();
  }
  // /pulls/{n}/files
  if (rest.length === 2 && rest[1] === 'files' && req.method === 'GET') {
    store.refreshPullHead(repo, pull);
    return json(
      200,
      pull.files.map((f) => ({
        filename: f.filename,
        status: f.status === 'removed' ? 'modified' : f.status,
        additions: f.additions,
        deletions: f.deletions,
        changes: f.changes,
      })),
    );
  }
  // /pulls/{n}/merge
  if (rest.length === 2 && rest[1] === 'merge' && req.method === 'PUT') {
    if (pull.state === 'closed') return err(405, `Pull request #${number} is already closed`);
    store.refreshPullHead(repo, pull);
    if (pull.mergeable === 'CONFLICTING') return err(405, `Pull request #${number} has conflicts`);
    const author = `${VIEWER.name} <${VIEWER.email}>`;
    const message = `Merge pull request #${number} from ${repo.owner}/${pull.head.ref}`;
    const sha = store.squashMerge(repo, pull, message, author);
    pull.merged = true;
    pull.mergedAt = store.nowIso();
    pull.mergeCommitSha = sha;
    pull.state = 'closed';
    pull.updatedAt = store.nowIso();
    return json(200, { sha, merged: true, message });
  }
  return notFound();
}
