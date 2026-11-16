# git-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Richkys1/git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Richkys1/git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in one Node process: git smart HTTP (upload-pack
protocol v2, receive-pack v0), REST v3, GraphQL v4 and MCP — no git binary, no
outside service, everything served from its own content-addressed store.

## Run it

```sh
node main.ts
```

`main.ts` binds an ephemeral port, registers it with `$MAKEFUN_ENV_URL`, then
polls `/done` until the suite finishes. `app.ts` is the server: repositories,
issues, pull requests, labels, contents and git objects, all in memory and on
disk under the store directory.

---

<sub>Entered on [make.fun](https://make.fun/?ref=xmc7n7gx&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
