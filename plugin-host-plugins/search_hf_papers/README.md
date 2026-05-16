# dr-plugin-search-hf-papers

Hugging Face Papers search plugin for the deepresearch plugin-host.

Wraps the legacy `app/adapters/hf_papers.py` adapter behind the
`search.query` / `search.fetch` plugin RPC surface. Engagement-curated
daily index over arXiv; `fetch()` resolves to `arxiv.org/pdf/{id}` so
the trust pipeline sees `source_tier=pdf`.

## Install (editable)

```bash
pip install -e ../_sdk
pip install -e .
python -m pytest -q
```

## Config

No required env vars. The HF papers search endpoint is public/keyless.
