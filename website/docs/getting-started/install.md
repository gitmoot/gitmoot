# Install

Install the latest Gitmoot beta:

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
gitmoot version
```

Gitmoot depends on Git and GitHub CLI for repository and PR workflows:

```sh
git --version
gh auth status
```

Install runtime plugin guidance when you want Codex or Claude Code to discover
the Gitmoot skill:

```sh
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
```

The plugins are discovery and guidance surfaces. The `gitmoot` CLI and local
daemon remain the execution path.

