// The entry <-> env lifecycle, as env/src/wire.ts settles it: bind an ephemeral port, tell the env the
// port it bound, then poll until the env says the trial is over and exit 0. The forge itself is the
// vendored static Go binary next to this file (go/forge, built by go/BUILD.md); this file only
// spawns it, learns its port from its stdout, and drives the lifecycle.
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial container). Without it
//      there is nothing to talk to, so the program exits 1 at once.
//   2. Spawn ./forge: it binds an ephemeral port of its own and prints
//      "[forge] listening on port N" as its first stdout line. Never bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address the request came from
//      plus N as your base URL and dials it from then on. The first register wins; a 409 means another
//      entry already registered, and this process keeps waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env answers at once ({"done":
//      false} while the suite runs), so this is a poll, not a long-held request. A failed or slow poll
//      is retried with a short exponential backoff; the process never exits before the env says done.
//   5. Stop the forge and exit 0. The harness then stops the env, which writes the export the scorer
//      reads.
//
// main.ts imports nothing but Node built-ins: no package, no model call.
import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

/** How long one register or done request may take before it is abandoned and retried. */
const REQUEST_TIMEOUT_MS = 30_000;
/** The first wait between retries, and the longest. */
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** True once SIGTERM or SIGINT has arrived: the process is winding down, not waiting. */
let windingDown = false;

/** The wait after `attempt` failed requests: exponential from RETRY_INITIAL_MS, capped. */
function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

/**
 * POST the bound port until the env accepts it. 200 is the normal answer; 409 means a first register
 * already won, which is not this process's to fix. 400 and 405 are contract errors and end the program;
 * a network error or another status is retried, since the env may still be starting.
 */
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

/** Poll GET /done until it answers {"done":true}; never return before that. */
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

/**
 * Start the forge binary and wait for its "[forge] listening on port N" line. The binary's
 * stdout is piped so the port can be read; every line is forwarded to this process's stdout.
 * A child that exits before announcing its port is a startup failure.
 */
function startForge(): Promise<{ child: ChildProcess; port: number }> {
  const forgePath = fileURLToPath(new URL('./forge', import.meta.url));
  const child = spawn(forgePath, [], { stdio: ['ignore', 'pipe', 'inherit'] });
  return new Promise((resolve, reject) => {
    let settled = false;
    const lines = createInterface({ input: child.stdout! });
    lines.on('line', (line) => {
      process.stdout.write(`${line}\n`);
      if (settled) return;
      const match = /\[forge\] listening on port (\d+)/.exec(line);
      if (match) {
        settled = true;
        resolve({ child, port: Number(match[1]) });
      }
    });
    child.once('error', (err) => {
      if (!settled) {
        settled = true;
        reject(new Error(`could not start the forge binary: ${err.message}`));
      }
    });
    child.once('exit', (code) => {
      if (!settled) {
        settled = true;
        reject(new Error(`the forge binary exited before listening (code ${code})`));
      }
    });
  });
}

/** Stop the forge child (SIGTERM, then SIGKILL if it lingers) and wait for it to leave. */
async function stopForge(child: ChildProcess): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  await new Promise<void>((resolve) => {
    const done = (): void => resolve();
    child.once('exit', done);
    child.kill('SIGTERM');
    setTimeout(() => {
      if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
    }, 2_000);
    // Never hang the shutdown on a stubborn child.
    setTimeout(done, 5_000);
  });
}

/** Spawn the forge, register its port, then wait for the env: the whole life of the process. */
async function main(): Promise<void> {
  const { child, port } = await startForge();
  process.stdout.write(`[entry] forge listening on port ${port}\n`);

  const windDown = async (): Promise<void> => {
    if (windingDown) return;
    windingDown = true;
    process.stdout.write('[entry] termination signal received; stopping the forge\n');
    await stopForge(child);
    process.stdout.write('[entry] forge stopped\n');
    process.exit(0);
  };
  process.once('SIGTERM', () => void windDown());
  process.once('SIGINT', () => void windDown());
  child.once('exit', (code) => {
    if (windingDown) return;
    // The harness stops the entry by signalling the whole process group;
    // the child may die a tick before main.ts sees its own signal. Give
    // the signal handler a moment to take over before treating this as a
    // real crash, so a clean shutdown doesn't look like a failure.
    setTimeout(() => {
      if (windingDown) return;
      process.stderr.write(`[entry] the forge exited unexpectedly (code ${code})\n`);
      process.exit(1);
    }, 1000).unref();
  });

  await register(port);
  process.stdout.write(`[entry] serving on port ${port} for ${ENV_URL}\n`);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await waitForDone();
  await stopForge(child);
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
