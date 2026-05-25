# Docs Deployment

The Gitmoot docs site is a static Docusaurus build, matching the operational
shape used by Entmoot docs.

Build from the repo root:

```sh
cd website
npm install
npm run build
```

The build output is `website/build`. For the current server, deploy it under
the existing `gitmoot.io` host:

```sh
rsync -a --delete website/build/ /var/www/gitmoot-docs/
```

Nginx serves:

- `https://gitmoot.io/docs/` from `/var/www/gitmoot-docs/`
- `https://gitmoot.io/llms.txt` from the docs build
- `https://gitmoot.io/llms-full.txt` from the docs build

Smoke checks:

```sh
curl -fsS https://gitmoot.io/docs/intro >/dev/null
curl -fsS https://gitmoot.io/llms.txt >/dev/null
curl -fsS https://gitmoot.io/llms-full.txt >/dev/null
```

`docs.gitmoot.io` currently resolves to this server but should only be enabled
after the origin TLS certificate and nginx server block explicitly cover that
host.

