// The forge's HTTP server: health probe, git smart HTTP, REST v3, GraphQL v4, MCP.
// Every reply carries a non-empty X-GitHub-Request-Id. Auth is one bearer token from
// MAKEFUN_SHOP_TOKEN (any non-empty bearer when the variable is unset, for local runs);
// without it, every protected route is 401 JSON.
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { handleInfoRefs, handleReceivePack, handleUploadPack } from './lib/gitsmart.ts';
import { executeGraphQL } from './lib/graphql.ts';
import { handleMcp } from './lib/mcp.ts';
import { handleRest, type RestRequest } from './lib/rest.ts';
import { Store } from './lib/store.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const MAX_BODY_BYTES = 32 * 1024 * 1024;

const store = new Store();
let requestCounter = 0;

function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter}`;
}

/** The bearer token the entry expects, or null when any non-empty bearer is accepted. */
function expectedToken(): string | null {
  const token = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  return token === '' ? null : token;
}

function authorized(req: IncomingMessage): boolean {
  const header = req.headers.authorization ?? '';
  const match = /^Bearer\s+(.+)$/.exec(header);
  if (!match) return false;
  const presented = match[1].trim();
  if (presented === '') return false;
  const expected = expectedToken();
  return expected === null || presented === expected;
}

function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  if (status === 204 || body === null || body === undefined) {
    res.writeHead(status, { 'x-github-request-id': nextRequestId(), ...headers });
    res.end();
    return;
  }
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text), 'x-github-request-id': nextRequestId(), ...headers });
  res.end(text);
}

function sendBuffer(res: ServerResponse, status: number, contentType: string, body: Buffer): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': body.length, 'x-github-request-id': nextRequestId() });
  res.end(body);
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        reject(new Error('request body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function segmentsOf(pathname: string): string[] {
  return pathname
    .split('/')
    .filter((s) => s !== '')
    .map((s) => {
      try {
        return decodeURIComponent(s);
      } catch {
        return s;
      }
    });
}

/** Try to pull a JSON-RPC id out of a malformed body, for the MCP parse-error reply. */
function extractRpcId(raw: string): unknown {
  const match = /"id"\s*:\s*("(?:[^"\\]|\\.)*"|-?\d+)/.exec(raw);
  if (!match) return null;
  try {
    return JSON.parse(match[1]);
  } catch {
    return null;
  }
}

export function createApp(): Server {
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      try {
        await handle(req, res);
      } catch (err) {
        try {
          sendJson(res, 500, { message: err instanceof Error ? err.message : 'internal error' });
        } catch {
          // The socket is gone; nothing more to do.
        }
      }
    })();
  });
}

async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const method = req.method ?? 'GET';
  let url: URL;
  try {
    url = new URL(req.url ?? '/', 'http://entry');
  } catch {
    sendJson(res, 400, { message: 'bad request url' });
    return;
  }
  const pathname = url.pathname;
  const segments = segmentsOf(pathname);

  // The env's boot check calls this twice: 200 {"ok":true}. No auth.
  if (method === 'GET' && pathname === '/healthz') {
    sendJson(res, 200, { ok: true });
    return;
  }

  // Everything else needs the bearer token.
  if (!authorized(req)) {
    sendJson(res, 401, { message: 'Requires authentication' });
    return;
  }

  const rawBody = await readBody(req);
  const host = req.headers.host ?? 'localhost';

  // REST v3 under /api/v3.
  if (pathname === '/api/v3' || pathname === '/api/v3/' || pathname.startsWith('/api/v3/')) {
    const apiSegments = segments[0] === 'api' && segments[1] === 'v3' ? segments.slice(2) : segments;
    let parsed: unknown;
    if (rawBody.length > 0) {
      try {
        parsed = JSON.parse(rawBody.toString('utf8'));
      } catch {
        sendJson(res, 400, { message: 'malformed JSON body' });
        return;
      }
    }
    const result = handleRest(store, {
      method,
      segments: apiSegments,
      query: url.searchParams,
      headers: req.headers as Record<string, string>,
      body: parsed,
      rawBody: rawBody.toString('utf8'),
      host,
    } satisfies RestRequest);
    sendJson(res, result.status, result.body, result.headers ?? {});
    return;
  }

  // GraphQL v4.
  if (pathname === '/api/graphql') {
    if (method !== 'POST') {
      sendJson(res, 405, { message: 'method not allowed' });
      return;
    }
    let parsed: unknown;
    try {
      parsed = rawBody.length > 0 ? JSON.parse(rawBody.toString('utf8')) : {};
    } catch {
      sendJson(res, 400, { errors: [{ message: 'malformed JSON body' }] });
      return;
    }
    const result = executeGraphQL(store, parsed);
    sendJson(res, result.status, result.body);
    return;
  }

  // MCP.
  if (pathname === '/mcp') {
    if (method !== 'POST') {
      sendJson(res, 405, { message: 'method not allowed' });
      return;
    }
    let parsed: unknown;
    try {
      parsed = rawBody.length > 0 ? JSON.parse(rawBody.toString('utf8')) : {};
    } catch {
      sendJson(res, 400, {
        jsonrpc: '2.0',
        id: extractRpcId(rawBody.toString('utf8')),
        error: { code: -32700, message: 'Parse error' },
      });
      return;
    }
    const result = handleMcp(store, parsed);
    if (result.status === 202 || result.body === null) {
      res.writeHead(202, { 'x-github-request-id': nextRequestId() });
      res.end();
      return;
    }
    sendJson(res, result.status, result.body);
    return;
  }

  // git smart HTTP: /{owner}/{repo}.git/info/refs | /{owner}/{repo}.git/git-upload-pack | /git-receive-pack
  if (segments.length >= 3 && segments[1].endsWith('.git')) {
    const owner = segments[0];
    const repoName = segments[1].slice(0, -'.git'.length);
    const action = segments.slice(2).join('/');
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/.test(owner) || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/.test(repoName)) {
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }
    if (method === 'GET' && action === 'info/refs') {
      const service = url.searchParams.get('service') ?? '';
      if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
        sendJson(res, 400, { message: 'service parameter is required' });
        return;
      }
      const gitProtocol = req.headers['git-protocol'];
      const result = handleInfoRefs(store, owner, repoName, service, typeof gitProtocol === 'string' ? gitProtocol : undefined);
      if (result.status === 404) {
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }
      sendBuffer(res, result.status, result.contentType, result.body);
      return;
    }
    if (method === 'POST' && action === 'git-upload-pack') {
      const gitProtocol = req.headers['git-protocol'];
      const result = handleUploadPack(store, owner, repoName, rawBody, typeof gitProtocol === 'string' ? gitProtocol : undefined);
      if (result.status === 404) {
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }
      if (result.status === 400) {
        sendBuffer(res, 400, result.contentType, result.body);
        return;
      }
      sendBuffer(res, result.status, result.contentType, result.body);
      return;
    }
    if (method === 'POST' && action === 'git-receive-pack') {
      const result = handleReceivePack(store, owner, repoName, rawBody);
      if (result.status === 404) {
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }
      if (result.status === 400) {
        sendBuffer(res, 400, result.contentType, result.body);
        return;
      }
      sendBuffer(res, result.status, result.contentType, result.body);
      return;
    }
  }

  sendJson(res, 404, { message: 'Not Found' });
}
