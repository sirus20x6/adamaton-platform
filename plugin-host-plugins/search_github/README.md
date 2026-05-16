# dr-plugin-search-github

GitHub search plugin for the deepresearch plugin-host.

Defaults to repository search; prefix the query with `code:` to switch
to code search. `fetch()` returns the matched repository's README as
markdown.

## Config

| Env var | Required | Effect |
|---|---|---|
| `GITHUB_TOKEN` | optional | lifts the unauth 60 req/hr rate cap to 5000 req/hr |
