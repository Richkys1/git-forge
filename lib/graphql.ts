// GraphQL v4 at /api/graphql: a small parser + resolver for the documents the suite sends
// (viewer, repository + issues/pulls connections, node, __type introspection, and the five
// mutations). Unknown fields are a GraphQL error (HTTP 200 + errors array); an unknown
// repository is data.repository === null with no errors.
import { decodeGlobalId, globalId, VIEWER, type Issue, type Pull, type Repo, type Store } from './store.ts';

// -------------------------------------------------------------------------------------------
// Lexer / parser for the GraphQL document subset the suite uses
// -------------------------------------------------------------------------------------------

type Value = string | number | boolean | null | Value[] | { [key: string]: Value };

interface FieldNode {
  kind: 'field';
  alias: string;
  name: string;
  args: Record<string, Value>;
  selections: Selection[];
}

interface FragmentNode {
  kind: 'fragment';
  typeCondition: string | null;
  selections: Selection[];
}

type Selection = FieldNode | FragmentNode;

interface Operation {
  kind: 'query' | 'mutation';
  name: string | null;
  selections: Selection[];
}

type Token =
  | { type: 'name'; value: string }
  | { type: 'string'; value: string }
  | { type: 'int'; value: number }
  | { type: 'punct'; value: string }
  | { type: 'spread'; value: '...' };

function tokenize(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < source.length) {
    const ch = source[i];
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && source.slice(i, i + 3) === '...') {
      tokens.push({ type: 'spread', value: '...' });
      i += 3;
      continue;
    }
    if ('{}()[]:!=$@&|'.includes(ch)) {
      tokens.push({ type: 'punct', value: ch });
      i += 1;
      continue;
    }
    if (ch === '"') {
      let j = i + 1;
      let out = '';
      while (j < source.length && source[j] !== '"') {
        if (source[j] === '\\' && j + 1 < source.length) {
          const esc = source[j + 1];
          out += esc === 'n' ? '\n' : esc === 't' ? '\t' : esc === 'r' ? '\r' : esc;
          j += 2;
        } else {
          out += source[j];
          j += 1;
        }
      }
      tokens.push({ type: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      let j = i;
      while (j < source.length && /[0-9.]/.test(source[j])) j += 1;
      tokens.push({ type: 'int', value: Number(source.slice(i, j)) });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let j = i;
      while (j < source.length && /[A-Za-z0-9_]/.test(source[j])) j += 1;
      tokens.push({ type: 'name', value: source.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(ch)} in GraphQL document`);
  }
  return tokens;
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token | undefined {
    return this.tokens[this.pos];
  }

  private next(): Token {
    const tok = this.tokens[this.pos];
    if (!tok) throw new Error('unexpected end of GraphQL document');
    this.pos += 1;
    return tok;
  }

  private expectPunct(value: string): void {
    const tok = this.next();
    if (tok.type !== 'punct' || tok.value !== value) throw new Error(`expected ${value}, got ${JSON.stringify(tok)}`);
  }

  private expectName(): string {
    const tok = this.next();
    if (tok.type !== 'name') throw new Error(`expected a name, got ${JSON.stringify(tok)}`);
    return tok.value;
  }

  parseDocument(): Operation[] {
    const operations: Operation[] = [];
    while (this.pos < this.tokens.length) {
      const tok = this.peek()!;
      if (tok.type === 'punct' && tok.value === '{') {
        operations.push({ kind: 'query', name: null, selections: this.parseSelectionSet() });
        continue;
      }
      if (tok.type === 'name' && (tok.value === 'query' || tok.value === 'mutation')) {
        const kind = tok.value as 'query' | 'mutation';
        this.next();
        let name: string | null = null;
        const next = this.peek();
        if (next?.type === 'name') name = this.next().value as string;
        // Variable definitions: ($name: Type, ...) — skip, we only need the names at runtime.
        if (this.peek()?.type === 'punct' && (this.peek() as { value: string }).value === '(') {
          let depth = 0;
          while (this.pos < this.tokens.length) {
            const t = this.next();
            if (t.type === 'punct' && t.value === '(') depth += 1;
            if (t.type === 'punct' && t.value === ')') {
              depth -= 1;
              if (depth === 0) break;
            }
          }
        }
        operations.push({ kind, name, selections: this.parseSelectionSet() });
        continue;
      }
      throw new Error(`unexpected token ${JSON.stringify(tok)} at top level of GraphQL document`);
    }
    return operations;
  }

  private parseSelectionSet(): Selection[] {
    this.expectPunct('{');
    const selections: Selection[] = [];
    while (true) {
      const tok = this.peek();
      if (!tok) throw new Error('unterminated selection set');
      if (tok.type === 'punct' && tok.value === '}') {
        this.next();
        return selections;
      }
      if (tok.type === 'spread') {
        this.next();
        let typeCondition: string | null = null;
        const next = this.peek();
        if (next?.type === 'name' && next.value === 'on') {
          this.next();
          typeCondition = this.expectName();
        }
        selections.push({ kind: 'fragment', typeCondition, selections: this.parseSelectionSet() });
        continue;
      }
      selections.push(this.parseField());
    }
  }

  private parseField(): FieldNode {
    let name = this.expectName();
    let alias = name;
    const colon = this.peek();
    if (colon?.type === 'punct' && colon.value === ':') {
      this.next();
      alias = name;
      name = this.expectName();
    }
    const args: Record<string, Value> = {};
    if (this.peek()?.type === 'punct' && (this.peek() as { value: string }).value === '(') {
      this.next();
      while (true) {
        const tok = this.peek();
        if (!tok) throw new Error('unterminated argument list');
        if (tok.type === 'punct' && tok.value === ')') {
          this.next();
          break;
        }
        const argName = this.expectName();
        this.expectPunct(':');
        args[argName] = this.parseValue();
      }
    }
    let selections: Selection[] = [];
    if (this.peek()?.type === 'punct' && (this.peek() as { value: string }).value === '{') {
      selections = this.parseSelectionSet();
    }
    return { kind: 'field', alias, name, args, selections };
  }

  private parseValue(): Value {
    const tok = this.next();
    if (tok.type === 'string') return tok.value;
    if (tok.type === 'int') return tok.value;
    if (tok.type === 'name') {
      if (tok.value === 'true') return true;
      if (tok.value === 'false') return false;
      if (tok.value === 'null') return null;
      return { __enum: tok.value } as unknown as Value;
    }
    if (tok.type === 'punct' && tok.value === '$') {
      const name = this.expectName();
      return { __var: name } as unknown as Value;
    }
    if (tok.type === 'punct' && tok.value === '[') {
      const items: Value[] = [];
      while (true) {
        const next = this.peek();
        if (!next) throw new Error('unterminated list');
        if (next.type === 'punct' && next.value === ']') {
          this.next();
          return items;
        }
        items.push(this.parseValue());
      }
    }
    if (tok.type === 'punct' && tok.value === '{') {
      const obj: Record<string, Value> = {};
      while (true) {
        const next = this.peek();
        if (!next) throw new Error('unterminated input object');
        if (next.type === 'punct' && next.value === '}') {
          this.next();
          return obj;
        }
        const key = this.expectName();
        this.expectPunct(':');
        obj[key] = this.parseValue();
      }
    }
    throw new Error(`unexpected value token ${JSON.stringify(tok)}`);
  }
}

function resolveValue(value: Value, variables: Record<string, unknown>): unknown {
  if (value !== null && typeof value === 'object') {
    if ('__var' in value) return variables[(value as { __var: string }).__var] ?? null;
    if ('__enum' in value) return (value as { __enum: string }).__enum;
    if (Array.isArray(value)) return value.map((v) => resolveValue(v, variables));
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value)) out[k] = resolveValue(v, variables);
    return out;
  }
  return value;
}

function resolveArgs(args: Record<string, Value>, variables: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(args)) out[k] = resolveValue(v, variables);
  return out;
}

// -------------------------------------------------------------------------------------------
// Introspection type table
// -------------------------------------------------------------------------------------------

interface TypeDef {
  kind: 'OBJECT' | 'ENUM';
  fields?: string[];
  values?: string[];
}

const TYPE_TABLE: Record<string, TypeDef> = {
  Query: { kind: 'OBJECT', fields: ['viewer', 'repository', 'node', '__type', '__schema'] },
  Mutation: { kind: 'OBJECT', fields: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'] },
  User: { kind: 'OBJECT', fields: ['id', 'login', 'name', 'email'] },
  Repository: {
    kind: 'OBJECT',
    fields: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequest', 'pullRequests', 'description', 'isPrivate', 'createdAt', 'updatedAt'],
  },
  RepositoryOwner: { kind: 'OBJECT', fields: ['id', 'login'] },
  Ref: { kind: 'OBJECT', fields: ['id', 'name'] },
  Issue: { kind: 'OBJECT', fields: ['id', 'number', 'title', 'body', 'state', 'createdAt', 'updatedAt', 'author', 'labels', 'comments', 'repository', 'url'] },
  IssueConnection: { kind: 'OBJECT', fields: ['nodes', 'edges', 'pageInfo', 'totalCount'] },
  IssueEdge: { kind: 'OBJECT', fields: ['node', 'cursor'] },
  PullRequest: { kind: 'OBJECT', fields: ['id', 'number', 'title', 'body', 'state', 'headRefName', 'baseRefName', 'headRef', 'baseRef', 'mergeable', 'merged', 'mergeCommit', 'files', 'author', 'createdAt', 'updatedAt', 'repository'] },
  PullRequestConnection: { kind: 'OBJECT', fields: ['nodes', 'edges', 'pageInfo', 'totalCount'] },
  PullRequestEdge: { kind: 'OBJECT', fields: ['node', 'cursor'] },
  PullRequestFileConnection: { kind: 'OBJECT', fields: ['nodes', 'edges', 'pageInfo', 'totalCount'] },
  PullRequestFile: { kind: 'OBJECT', fields: ['path', 'additions', 'deletions', 'changeType'] },
  PageInfo: { kind: 'OBJECT', fields: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'] },
  Comment: { kind: 'OBJECT', fields: ['id', 'body', 'author', 'createdAt', 'updatedAt'] },
  Commit: { kind: 'OBJECT', fields: ['oid', 'message', 'author', 'committer'] },
  CommentEdge: { kind: 'OBJECT', fields: ['node', 'cursor'] },
  AddCommentPayload: { kind: 'OBJECT', fields: ['commentEdge', 'clientMutationId'] },
  CreateIssuePayload: { kind: 'OBJECT', fields: ['issue', 'clientMutationId'] },
  CloseIssuePayload: { kind: 'OBJECT', fields: ['issue', 'clientMutationId'] },
  CreatePullRequestPayload: { kind: 'OBJECT', fields: ['pullRequest', 'clientMutationId'] },
  MergePullRequestPayload: { kind: 'OBJECT', fields: ['pullRequest', 'clientMutationId'] },
  __Type: { kind: 'OBJECT', fields: ['name', 'kind', 'fields', 'enumValues', 'description'] },
  __Field: { kind: 'OBJECT', fields: ['name', 'description', 'type'] },
  __EnumValue: { kind: 'OBJECT', fields: ['name', 'description'] },
  IssueState: { kind: 'ENUM', values: ['OPEN', 'CLOSED'] },
  MergeableState: { kind: 'ENUM', values: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'] },
  PullRequestState: { kind: 'ENUM', values: ['OPEN', 'CLOSED', 'MERGED'] },
  PullRequestMergeMethod: { kind: 'ENUM', values: ['MERGE', 'SQUASH', 'REBASE'] },
  RepositoryPermission: { kind: 'ENUM', values: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'] },
  SearchType: { kind: 'ENUM', values: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'] },
  IssueOrderField: { kind: 'ENUM', values: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'] },
  OrderDirection: { kind: 'ENUM', values: ['ASC', 'DESC'] },
};

// -------------------------------------------------------------------------------------------
// Execution
// -------------------------------------------------------------------------------------------

interface GqlObject {
  __typename: string;
  [key: string]: unknown;
}

interface Ctx {
  store: Store;
}

interface ExecResult {
  data: unknown;
  errors?: { message: string }[];
}

function cursorFor(issue: Issue): string {
  return Buffer.from(`cursor:${issue.number}`, 'utf8').toString('base64');
}

function issueFromCursor(repo: Repo, cursor: string | null): Issue | null {
  if (!cursor) return null;
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    if (!text.startsWith('cursor:')) return null;
    const number = Number(text.slice('cursor:'.length));
    return repo.issues.get(number) ?? null;
  } catch {
    return null;
  }
}

function repoObject(repo: Repo): GqlObject {
  return { __typename: 'Repository', __repo: repo };
}

function issueObject(repo: Repo, issue: Issue): GqlObject {
  return { __typename: 'Issue', __repo: repo, __issue: issue };
}

function pullObject(repo: Repo, pull: Pull): GqlObject {
  return { __typename: 'PullRequest', __repo: repo, __pull: pull };
}

function issuesConnection(repo: Repo, args: Record<string, unknown>): GqlObject {
  const first = typeof args.first === 'number' ? args.first : 10;
  const after = typeof args.after === 'string' ? args.after : null;
  const states = Array.isArray(args.states) ? (args.states as string[]) : null;
  const orderBy = (args.orderBy ?? {}) as { field?: string; direction?: string };
  let issues = [...repo.issues.values()];
  if (states) {
    const wanted = new Set(states.map((s) => s.toLowerCase()));
    issues = issues.filter((i) => wanted.has(i.state));
  }
  const direction = (orderBy.direction ?? 'DESC').toUpperCase();
  issues.sort((a, b) => (a.number < b.number ? -1 : a.number > b.number ? 1 : 0));
  if (direction === 'DESC') issues.reverse();
  if (after) {
    const anchor = issueFromCursor(repo, after);
    if (anchor) {
      const idx = issues.findIndex((i) => i.number === anchor.number);
      if (idx >= 0) issues = issues.slice(idx + 1);
    }
  }
  const page = issues.slice(0, first);
  const hasNextPage = issues.length > first;
  const endCursor = page.length > 0 ? cursorFor(page[page.length - 1]) : null;
  return {
    __typename: 'IssueConnection',
    __nodes: page.map((i) => issueObject(repo, i)),
    __pageInfo: { hasNextPage, hasPreviousPage: false, startCursor: page.length > 0 ? cursorFor(page[0]) : null, endCursor },
  };
}

function pullsConnection(repo: Repo, args: Record<string, unknown>): GqlObject {
  const first = typeof args.first === 'number' ? args.first : 10;
  const pulls = [...repo.pulls.values()].sort((a, b) => b.number - a.number).slice(0, first);
  return {
    __typename: 'PullRequestConnection',
    __nodes: pulls.map((p) => pullObject(repo, p)),
    __pageInfo: { hasNextPage: false, hasPreviousPage: false, startCursor: null, endCursor: null },
  };
}

function findByGlobalId(store: Store, id: string): GqlObject | null {
  const decoded = decodeGlobalId(id);
  if (!decoded) return null;
  if (decoded.kind === 'Repository') {
    const repoId = Number(decoded.parts[0]);
    for (const repo of store.repos.values()) if (repo.id === repoId) return repoObject(repo);
    return null;
  }
  if (decoded.kind === 'Issue') {
    const repoId = Number(decoded.parts[0]);
    const number = Number(decoded.parts[1]);
    for (const repo of store.repos.values()) {
      if (repo.id === repoId) {
        const issue = repo.issues.get(number);
        return issue ? issueObject(repo, issue) : null;
      }
    }
    return null;
  }
  if (decoded.kind === 'PullRequest') {
    const repoId = Number(decoded.parts[0]);
    const number = Number(decoded.parts[1]);
    for (const repo of store.repos.values()) {
      if (repo.id === repoId) {
        const pull = repo.pulls.get(number);
        return pull ? pullObject(repo, pull) : null;
      }
    }
    return null;
  }
  return null;
}

function fieldValue(obj: GqlObject, field: string, args: Record<string, unknown>, ctx: Ctx): unknown {
  const store = ctx.store;
  switch (obj.__typename) {
    case 'Query': {
      if (field === 'viewer') return { __typename: 'User', __user: VIEWER };
      if (field === 'repository') {
        const owner = String(args.owner ?? '');
        const name = String(args.name ?? '');
        if (owner !== VIEWER.login) return null;
        const repo = store.getRepo(owner, name);
        return repo ? repoObject(repo) : null;
      }
      if (field === 'node') return findByGlobalId(store, String(args.id ?? ''));
      if (field === '__type') {
        const def = TYPE_TABLE[String(args.name ?? '')];
        if (!def) return null;
        return { __typename: '__Type', __typeName: String(args.name), __def: def };
      }
      return undefined;
    }
    case 'Mutation': {
      return mutationField(obj, field, args, ctx);
    }
    case 'User': {
      const user = obj.__user as typeof VIEWER;
      if (field === 'login') return user.login;
      if (field === 'id') return globalId('User', user.id);
      if (field === 'name') return user.name;
      if (field === 'email') return user.email;
      return undefined;
    }
    case 'Repository': {
      const repo = obj.__repo as Repo;
      if (field === 'id') return globalId('Repository', repo.id);
      if (field === 'databaseId') return repo.id;
      if (field === 'name') return repo.name;
      if (field === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
      if (field === 'owner') return { __typename: 'RepositoryOwner', __login: repo.owner };
      if (field === 'defaultBranchRef') return { __typename: 'Ref', __name: repo.defaultBranch };
      if (field === 'viewerPermission') return 'ADMIN';
      if (field === 'hasIssuesEnabled') return true;
      if (field === 'description') return repo.description;
      if (field === 'isPrivate') return repo.private;
      if (field === 'createdAt') return repo.createdAt;
      if (field === 'updatedAt') return repo.updatedAt;
      if (field === 'issue') {
        const issue = repo.issues.get(Number(args.number));
        return issue ? issueObject(repo, issue) : null;
      }
      if (field === 'issues') return issuesConnection(repo, args);
      if (field === 'pullRequest') {
        const pull = repo.pulls.get(Number(args.number));
        return pull ? pullObject(repo, pull) : null;
      }
      if (field === 'pullRequests') return pullsConnection(repo, args);
      return undefined;
    }
    case 'RepositoryOwner': {
      if (field === 'login') return obj.__login as string;
      if (field === 'id') return globalId('User', VIEWER.id);
      return undefined;
    }
    case 'Ref': {
      if (field === 'name') return obj.__name as string;
      if (field === 'id') return globalId('Ref', obj.__name as string);
      return undefined;
    }
    case 'Issue': {
      const issue = obj.__issue as Issue;
      const repo = obj.__repo as Repo;
      if (field === 'id') return issue.id;
      if (field === 'number') return issue.number;
      if (field === 'title') return issue.title;
      if (field === 'body') return issue.body;
      if (field === 'state') return issue.state.toUpperCase();
      if (field === 'createdAt') return issue.createdAt;
      if (field === 'updatedAt') return issue.updatedAt;
      if (field === 'repository') return repoObject(repo);
      if (field === 'author') return { __typename: 'User', __user: VIEWER };
      return undefined;
    }
    case 'IssueConnection':
    case 'PullRequestConnection':
    case 'PullRequestFileConnection': {
      if (field === 'nodes') return obj.__nodes as GqlObject[];
      if (field === 'edges') return (obj.__nodes as GqlObject[]).map((n) => ({ __typename: 'IssueEdge', __node: n, __cursor: '' }));
      if (field === 'pageInfo') {
        const info = obj.__pageInfo as Record<string, unknown>;
        return { __typename: 'PageInfo', ...info };
      }
      if (field === 'totalCount') return (obj.__nodes as GqlObject[]).length;
      return undefined;
    }
    case 'IssueEdge':
    case 'PullRequestEdge':
    case 'CommentEdge': {
      if (field === 'node') return obj.__node as GqlObject;
      if (field === 'cursor') return obj.__cursor as string;
      return undefined;
    }
    case 'PullRequest': {
      const pull = obj.__pull as Pull;
      const repo = obj.__repo as Repo;
      if (field === 'id') return pull.id;
      if (field === 'number') return pull.number;
      if (field === 'title') return pull.title;
      if (field === 'body') return pull.body;
      if (field === 'state') return pull.merged ? 'MERGED' : pull.state.toUpperCase();
      if (field === 'headRefName') return pull.head.ref;
      if (field === 'baseRefName') return pull.base.ref;
      if (field === 'headRef') return { __typename: 'Ref', __name: pull.head.ref };
      if (field === 'baseRef') return { __typename: 'Ref', __name: pull.base.ref };
      if (field === 'mergeable') return pull.mergeable;
      if (field === 'merged') return pull.merged;
      if (field === 'mergeCommit') return pull.mergeCommitSha ? { __typename: 'Commit', __sha: pull.mergeCommitSha } : null;
      if (field === 'createdAt') return pull.createdAt;
      if (field === 'updatedAt') return pull.updatedAt;
      if (field === 'repository') return repoObject(repo);
      if (field === 'author') return { __typename: 'User', __user: VIEWER };
      if (field === 'files') {
        const first = typeof args.first === 'number' ? args.first : 10;
        const nodes = pull.files.slice(0, first).map((f) => ({ __typename: 'PullRequestFile', __file: f }));
        return { __typename: 'PullRequestFileConnection', __nodes: nodes, __pageInfo: { hasNextPage: false, hasPreviousPage: false, startCursor: null, endCursor: null } };
      }
      return undefined;
    }
    case 'PullRequestFile': {
      const file = obj.__file as Pull['files'][number];
      if (field === 'path') return file.filename;
      if (field === 'additions') return file.additions;
      if (field === 'deletions') return file.deletions;
      if (field === 'changeType') return file.status.toUpperCase();
      return undefined;
    }
    case 'PageInfo': {
      if (field === 'hasNextPage') return obj.hasNextPage as boolean;
      if (field === 'hasPreviousPage') return obj.hasPreviousPage as boolean;
      if (field === 'startCursor') return obj.startCursor as string | null;
      if (field === 'endCursor') return obj.endCursor as string | null;
      return undefined;
    }
    case 'Comment': {
      if (field === 'id') return obj.__commentId as string;
      if (field === 'body') return obj.__commentBody as string;
      if (field === 'author') return { __typename: 'User', __user: VIEWER };
      if (field === 'createdAt') return obj.__commentCreatedAt as string;
      if (field === 'updatedAt') return obj.__commentCreatedAt as string;
      return undefined;
    }
    case '__Type': {
      const def = obj.__def as TypeDef;
      if (field === 'name') return obj.__typeName as string;
      if (field === 'kind') return def.kind;
      if (field === 'description') return null;
      if (field === 'fields') {
        if (def.kind !== 'OBJECT') return null;
        return (def.fields ?? []).map((name) => ({ __typename: '__Field', __fieldName: name }));
      }
      if (field === 'enumValues') {
        if (def.kind !== 'ENUM') return null;
        return (def.values ?? []).map((name) => ({ __typename: '__EnumValue', __enumName: name }));
      }
      return undefined;
    }
    case '__Field': {
      if (field === 'name') return obj.__fieldName as string;
      if (field === 'description') return null;
      if (field === 'type') return { __typename: '__Type', __typeName: 'String', __def: { kind: 'OBJECT', fields: [] } };
      return undefined;
    }
    case '__EnumValue': {
      if (field === 'name') return obj.__enumName as string;
      if (field === 'description') return null;
      return undefined;
    }
    case 'CreateIssuePayload':
    case 'CloseIssuePayload': {
      if (field === 'issue') return obj.__issue as GqlObject;
      return undefined;
    }
    case 'CreatePullRequestPayload':
    case 'MergePullRequestPayload': {
      if (field === 'pullRequest') return obj.__pull as GqlObject;
      return undefined;
    }
    case 'AddCommentPayload': {
      if (field === 'commentEdge') return obj.__edge as GqlObject;
      return undefined;
    }
    default:
      return undefined;
  }
}

function mutationField(obj: GqlObject, field: string, args: Record<string, unknown>, ctx: Ctx): unknown {
  const store = ctx.store;
  const input = (args.input ?? {}) as Record<string, unknown>;
  if (field === 'createIssue') {
    const target = findByGlobalId(store, String(input.repositoryId ?? ''));
    if (!target || target.__typename !== 'Repository') return { __typename: 'CreateIssuePayload', __issue: null };
    const repo = target.__repo as Repo;
    const issue = store.createIssue(repo, String(input.title ?? ''), String(input.body ?? ''), [], VIEWER.login);
    return { __typename: 'CreateIssuePayload', __issue: issueObject(repo, issue) };
  }
  if (field === 'closeIssue') {
    const target = findByGlobalId(store, String(input.issueId ?? ''));
    if (!target || target.__typename !== 'Issue') return { __typename: 'CloseIssuePayload', __issue: null };
    const issue = target.__issue as Issue;
    issue.state = 'closed';
    issue.updatedAt = store.nowIso();
    return { __typename: 'CloseIssuePayload', __issue: target };
  }
  if (field === 'createPullRequest') {
    const target = findByGlobalId(store, String(input.repositoryId ?? ''));
    if (!target || target.__typename !== 'Repository') return { __typename: 'CreatePullRequestPayload', __pull: null };
    const repo = target.__repo as Repo;
    const pull = store.createPull(repo, {
      title: String(input.title ?? ''),
      body: String(input.body ?? ''),
      head: String(input.headRefName ?? ''),
      base: String(input.baseRefName ?? repo.defaultBranch),
      user: VIEWER.login,
    });
    return { __typename: 'CreatePullRequestPayload', __pull: pull ? pullObject(repo, pull) : null };
  }
  if (field === 'mergePullRequest') {
    const target = findByGlobalId(store, String(input.pullRequestId ?? ''));
    if (!target || target.__typename !== 'PullRequest') return { __typename: 'MergePullRequestPayload', __pull: null };
    const repo = target.__repo as Repo;
    const pull = target.__pull as Pull;
    if (pull.state === 'open') {
      store.refreshPullHead(repo, pull);
      if (pull.mergeable !== 'CONFLICTING') {
        const author = `${VIEWER.name} <${VIEWER.email}>`;
        const sha = store.squashMerge(repo, pull, `Merge pull request #${pull.number} from ${repo.owner}/${pull.head.ref}`, author);
        pull.merged = true;
        pull.mergedAt = store.nowIso();
        pull.mergeCommitSha = sha;
        pull.state = 'closed';
        pull.updatedAt = store.nowIso();
      }
    }
    return { __typename: 'MergePullRequestPayload', __pull: target };
  }
  if (field === 'addComment') {
    const target = findByGlobalId(store, String(input.subjectId ?? ''));
    if (!target || target.__typename !== 'Issue') return { __typename: 'AddCommentPayload', __edge: null };
    const issue = target.__issue as Issue;
    const repo = target.__repo as Repo;
    const comment = {
      id: repo.nextCommentId++,
      body: String(input.body ?? ''),
      user: VIEWER.login,
      createdAt: store.nowIso(),
      updatedAt: store.nowIso(),
    };
    issue.comments.push(comment);
    const node: GqlObject = {
      __typename: 'Comment',
      __commentId: globalId('IssueComment', repo.id, issue.number, comment.id),
      __commentBody: comment.body,
      __commentCreatedAt: comment.createdAt,
    };
    return { __typename: 'AddCommentPayload', __edge: { __typename: 'CommentEdge', __node: node, __cursor: '' } };
  }
  return undefined;
}

function resolveSelections(
  obj: GqlObject,
  selections: Selection[],
  variables: Record<string, unknown>,
  ctx: Ctx,
  errors: { message: string }[],
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of selections) {
    if (sel.kind === 'fragment') {
      if (sel.typeCondition !== null && sel.typeCondition !== obj.__typename) continue;
      Object.assign(out, resolveSelections(obj, sel.selections, variables, ctx, errors));
      continue;
    }
    const def = TYPE_TABLE[obj.__typename];
    if (def?.kind === 'OBJECT' && !def.fields?.includes(sel.name) && sel.name !== '__typename') {
      errors.push({ message: `Cannot query field "${sel.name}" on type "${obj.__typename}".` });
      continue;
    }
    if (sel.name === '__typename') {
      out[sel.alias] = obj.__typename;
      continue;
    }
    const args = resolveArgs(sel.args, variables);
    const value = fieldValue(obj, sel.name, args, ctx);
    if (value === undefined) {
      errors.push({ message: `Cannot query field "${sel.name}" on type "${obj.__typename}".` });
      continue;
    }
    out[sel.alias] = resolveValueNode(value, sel.selections, variables, ctx, errors);
  }
  return out;
}

function resolveValueNode(
  value: unknown,
  selections: Selection[],
  variables: Record<string, unknown>,
  ctx: Ctx,
  errors: { message: string }[],
): unknown {
  if (value === null || value === undefined) return value;
  if (Array.isArray(value)) return value.map((v) => resolveValueNode(v, selections, variables, ctx, errors));
  if (typeof value === 'object' && '__typename' in (value as Record<string, unknown>)) {
    if (selections.length === 0) return {};
    return resolveSelections(value as GqlObject, selections, variables, ctx, errors);
  }
  return value;
}

export function executeGraphQL(
  store: Store,
  body: unknown,
): { status: number; body: unknown } {
  const ctx: Ctx = { store };
  const requests = Array.isArray(body) ? body : [body];
  const results = requests.map((req) => executeOne(ctx, req));
  return { status: 200, body: Array.isArray(body) ? results : results[0] };
}

function executeOne(ctx: Ctx, req: unknown): ExecResult {
  const input = (req ?? {}) as { query?: unknown; variables?: unknown; operationName?: unknown };
  if (typeof input.query !== 'string') {
    return { data: null, errors: [{ message: 'GraphQL query must be a string.' }] };
  }
  let operations: Operation[];
  try {
    operations = new Parser(tokenize(input.query)).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : String(err) }] };
  }
  const variables = (input.variables ?? {}) as Record<string, unknown>;
  let op: Operation | undefined;
  if (typeof input.operationName === 'string') {
    op = operations.find((o) => o.name === input.operationName);
  } else if (operations.length === 1) {
    op = operations[0];
  } else {
    op = operations[0];
  }
  if (!op) return { data: null, errors: [{ message: 'No operation to execute.' }] };
  const rootType = op.kind === 'mutation' ? 'Mutation' : 'Query';
  const root: GqlObject = { __typename: rootType };
  const errors: { message: string }[] = [];
  const data = resolveSelections(root, op.selections, variables, ctx, errors);
  if (errors.length > 0) return { data: null, errors };
  return { data };
}
