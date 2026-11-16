# CAVU

CAVU stands for "Containerized Agents, Verified, Unattended". In aviation the same letters mean "ceiling and visibility unlimited".

CAVU is a self-hosted tool that runs a coding agent fully unattended. A task goes in, a pull request comes out, and a person merges it. The agent runs inside a container, its credentials stay outside it, and a run only counts as successful when a verify command, run by CAVU itself, passes.

## Status

Pre-release. Today the `cavu` command builds and `cavu version` prints its version. Nothing else works yet.

## Licence

MIT. See [LICENSE](LICENSE).
