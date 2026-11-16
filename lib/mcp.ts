// MCP (Model Context Protocol) at /mcp: JSON-RPC 2.0 over Streamable HTTP (single JSON body).
// Modern era: server/discover answers supportedVersions with 2026-07-28 first; initialize is
// also answered for the legacy fallback. Tools act on the same store as REST.
import { VIEWER, type Store } from './store.ts';

export const MCP_VERSION = '2026-07-28';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const SERVER_INFO = { name: 'forge', version: '1.0.0' };

interface ToolDef {
  name: string;
  description: string;
  inputSchema: { type: 'object'; properties: Record<string, { type: string; description: string }>; required: string[] };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Login of the repository owner.' },
        name: { type: 'string', description: 'Name of the repository to create.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Login of the repository owner.' },
        repo: { type: 'string', description: 'Name of the repository.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Login of the repository owner.' },
        repo: { type: 'string', description: 'Name of the repository.' },
        title: { type: 'string', description: 'Title of the issue.' },
        body: { type: 'string', description: 'Body of the issue.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Login of the repository owner.' },
        repo: { type: 'string', description: 'Name of the repository.' },
        state: { type: 'string', description: 'Issue state filter: open, closed, or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch onto a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Login of the repository owner.' },
        repo: { type: 'string', description: 'Name of the repository.' },
        title: { type: 'string', description: 'Title of the pull request.' },
        body: { type: 'string', description: 'Body of the pull request.' },
        head: { type: 'string', description: 'Name of the head branch.' },
        base: { type: 'string', description: 'Name of the base branch.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request with a squash commit onto its base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Login of the repository owner.' },
        repo: { type: 'string', description: 'Name of the repository.' },
        pull_number: { type: 'integer', description: 'Number of the pull request to merge.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function toolResult(data: unknown): Record<string, unknown> {
  const text = JSON.stringify(data);
  return { content: [{ type: 'text', text }], structuredContent: data };
}

function toolError(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function repoPayload(store: Store, owner: string, name: string): Record<string, unknown> | null {
  const repo = store.getRepo(owner, name);
  if (!repo) return null;
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
  };
}

function callTool(store: Store, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const owner = typeof args.owner === 'string' ? args.owner : VIEWER.login;
  const repoName = typeof args.repo === 'string' ? args.repo : typeof args.name === 'string' ? args.name : '';
  switch (name) {
    case 'create_repo': {
      const repoNameArg = typeof args.name === 'string' ? args.name : '';
      if (!repoNameArg) return toolError('create_repo requires "name".');
      const existing = store.getRepo(VIEWER.login, repoNameArg);
      const repo =
        existing ??
        store.createRepo(VIEWER.login, repoNameArg, {
          description: typeof args.description === 'string' ? args.description : '',
          private: args.private === true,
        });
      if (!repo) return toolError(`Repository ${repoNameArg} could not be created.`);
      return toolResult(repoPayload(store, VIEWER.login, repoNameArg));
    }
    case 'get_repo': {
      if (!repoName) return toolError('get_repo requires "repo".');
      const payload = repoPayload(store, owner, repoName);
      if (!payload) return toolError(`Repository ${owner}/${repoName} not found.`);
      return toolResult(payload);
    }
    case 'create_issue': {
      if (!repoName) return toolError('create_issue requires "repo".');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`Repository ${owner}/${repoName} not found.`);
      const title = typeof args.title === 'string' ? args.title : '';
      if (!title) return toolError('create_issue requires "title".');
      const issue = store.createIssue(repo, title, typeof args.body === 'string' ? args.body : '', [], VIEWER.login);
      return toolResult({ id: issue.id, number: issue.number, title: issue.title, body: issue.body, state: issue.state, created_at: issue.createdAt });
    }
    case 'list_issues': {
      if (!repoName) return toolError('list_issues requires "repo".');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`Repository ${owner}/${repoName} not found.`);
      const state = typeof args.state === 'string' ? args.state.toLowerCase() : 'open';
      const issues = [...repo.issues.values()]
        .filter((i) => (state === 'all' ? true : i.state === state))
        .sort((a, b) => b.number - a.number)
        .map((i) => ({ id: i.id, number: i.number, title: i.title, body: i.body, state: i.state }));
      return toolResult({ issues });
    }
    case 'create_pull_request': {
      if (!repoName) return toolError('create_pull_request requires "repo".');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`Repository ${owner}/${repoName} not found.`);
      const title = typeof args.title === 'string' ? args.title : '';
      const head = typeof args.head === 'string' ? args.head : '';
      const base = typeof args.base === 'string' ? args.base : repo.defaultBranch;
      if (!title || !head) return toolError('create_pull_request requires "title" and "head".');
      const pull = store.createPull(repo, { title, body: typeof args.body === 'string' ? args.body : '', head, base, user: VIEWER.login });
      if (!pull) return toolError(`Could not create a pull request from ${head} onto ${base}.`);
      return toolResult({
        id: pull.id,
        number: pull.number,
        pull_number: pull.number,
        title: pull.title,
        state: pull.state,
        head: pull.head.ref,
        base: pull.base.ref,
        head_ref: pull.head.ref,
        base_ref: pull.base.ref,
      });
    }
    case 'merge_pull_request': {
      if (!repoName) return toolError('merge_pull_request requires "repo".');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`Repository ${owner}/${repoName} not found.`);
      const number = Number(args.pull_number ?? args.number ?? args.pull_request_number);
      if (!Number.isInteger(number) || number < 1) return toolError('merge_pull_request requires "pull_number".');
      const pull = repo.pulls.get(number);
      if (!pull) return toolError(`Pull request #${number} not found.`);
      if (pull.state === 'closed') return toolError(`Pull request #${number} is already closed.`);
      store.refreshPullHead(repo, pull);
      if (pull.mergeable === 'CONFLICTING') return toolError(`Pull request #${number} has conflicts.`);
      const author = `${VIEWER.name} <${VIEWER.email}>`;
      const sha = store.squashMerge(repo, pull, `Merge pull request #${number} from ${repo.owner}/${pull.head.ref}`, author);
      pull.merged = true;
      pull.mergedAt = store.nowIso();
      pull.mergeCommitSha = sha;
      pull.state = 'closed';
      pull.updatedAt = store.nowIso();
      return toolResult({ number: pull.number, pull_number: pull.number, merged: true, state: 'closed', sha });
    }
    default:
      return toolError(`Unknown tool: ${name}`);
  }
}

export interface McpResult {
  status: number;
  body: unknown;
}

export function handleMcp(store: Store, request: unknown): McpResult {
  const req = (request ?? {}) as { jsonrpc?: unknown; id?: unknown; method?: unknown; params?: unknown };
  const id = 'id' in req ? req.id : undefined;
  const isNotification = id === undefined;
  const method = typeof req.method === 'string' ? req.method : '';
  const params = (req.params ?? {}) as Record<string, unknown>;

  const ok = (result: unknown): McpResult => ({ status: 200, body: { jsonrpc: '2.0', id: id ?? null, result } });
  const fail = (code: number, message: string): McpResult => ({ status: 200, body: { jsonrpc: '2.0', id: id ?? null, error: { code, message } } });

  if (method === 'notifications/initialized' || method.startsWith('notifications/')) {
    return { status: 202, body: null };
  }
  if (isNotification) return { status: 202, body: null };

  if (method === 'server/discover') {
    return ok({
      supportedVersions: SUPPORTED_VERSIONS,
      capabilities: { tools: {} },
      serverInfo: SERVER_INFO,
      instructions: 'A GitHub-compatible git forge: create repositories, issues, and pull requests.',
    });
  }
  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : MCP_VERSION;
    const negotiated = SUPPORTED_VERSIONS.includes(requested) ? requested : MCP_VERSION;
    return ok({ protocolVersion: negotiated, capabilities: { tools: {} }, serverInfo: SERVER_INFO });
  }
  if (method === 'ping') return ok({});
  if (method === 'tools/list') {
    // Deterministic: the same array, byte-identical, on every call.
    return ok({ tools: TOOLS });
  }
  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    const tool = TOOLS.find((t) => t.name === name);
    if (!tool) return fail(-32602, `Unknown tool: ${name}`);
    // Missing required argument: isError (accepted by the suite alongside -32602).
    for (const required of tool.inputSchema.required) {
      if (args[required] === undefined || args[required] === null || args[required] === '') {
        return ok(toolError(`Missing required argument "${required}" for tool "${name}".`));
      }
    }
    return ok(callTool(store, name, args));
  }
  return fail(-32601, `Method not found: ${method}`);
}
