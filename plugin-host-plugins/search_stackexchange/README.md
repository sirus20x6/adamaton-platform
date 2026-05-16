# dr-plugin-search-stackexchange

Stack Exchange search plugin for the deepresearch plugin-host.

`search()` calls `/search/advanced` with `filter=withbody`. `fetch()`
combines the question body with the top three voted answers (accepted
first) into a single markdown document.

## Config

| Env var | Required | Effect |
|---|---|---|
| `STACKEXCHANGE_KEY` | optional | API key; raises the daily request quota |
| `STACKEXCHANGE_SITE` | optional | site slug (default `stackoverflow`) |
