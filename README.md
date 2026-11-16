# CAVU

CAVU stands for "Containerized Agents, Verified, Unattended". In aviation the same letters mean "ceiling and visibility unlimited".

CAVU is meant to be a self-hosted tool that runs a coding agent fully unattended: a task goes in, a pull request comes out, and a person merges it. The agent is to run inside a container with API keys and forge tokens kept outside it, and a run is to count as successful only when a verify command, run by CAVU itself, passes.

## Status

Pre-release. Today `cavu version` prints the version, `cavu settings` sets and shows the settings, `cavu secret set` stores a secret read from standard input, `cavu image build` builds the sandbox image on Docker or Podman, and `cavu project` adds, changes and lists projects. Nothing else works yet.

## Licence

MIT. See [LICENSE](LICENSE).
