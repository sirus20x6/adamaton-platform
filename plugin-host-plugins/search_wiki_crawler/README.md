# search_wiki_crawler

BFS HTML crawlers ported from `app/adapters/wiki_crawler.py`. Three back-ends
(Lexicanum, TVTropes Warhammer40000, Warhammer Fantasy Fandom) share a uniform
`WikiPage` shape and BFS pattern but differ on transport (raw HTML vs MediaWiki
API) and politeness budget.

Query syntax for `search.query`:

```
<source>:<slug>     e.g.  lexicanum:Sigmar
                          tvtropes:/pmwiki/pmwiki.php/Main/SpaceMarines
                          fandom:Karl_Franz
<slug>              defaults to lexicanum
```

`limit` caps the BFS to `limit` pages; each visited page becomes one
`SearchResult` (URL = canonical wiki URL, abstract = first ~300 chars of the
markdown body, source_kind = WIKI). `search.fetch` re-runs the parser on the
single URL and returns the full markdown body in `FetchedDoc.body`.
