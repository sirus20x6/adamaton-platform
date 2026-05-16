# dr-plugin-search-wikipedia

Wikipedia search plugin for the deepresearch plugin-host.

`search()` uses the legacy opensearch endpoint (4-tuple of titles/descs/urls).
`fetch()` retrieves the page HTML through `/api/rest_v1/page/html/{title}`
and markdownifies it after stripping reference/infobox cruft.

## Config

| Env var | Required | Default | Effect |
|---|---|---|---|
| `WIKIPEDIA_LANGUAGE` | optional | `en` | Wikipedia subdomain (e.g. `de`, `fr`) |
