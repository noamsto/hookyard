# Redaction against the fake-secret matrix, per alternative

The same 17 fake cases as [`../redaction-cases.md`](../redaction-cases.md), run
through each alternative. `yes` = the value is absent from the tool's stored /
searchable output (replaced or rejected); `no` = it survives verbatim and is
recoverable. Every value is a placeholder; nothing here is a real credential.

| # | case (fake value) | recall | deja-vu | remem | claudemem |
| --- | --- | :-: | :-: | :-: | :-: |
| 1 | `AKIAFAKEFAKEFAKEFAKE` (AWS) | yes | yes | **no** | **no** |
| 2 | `ghp_…` (GitHub classic) | yes | yes | **no** | **no** |
| 3 | `github_pat_…` (GitHub fine-grained) | **no** | **no** | **no** | **no** |
| 4 | `sk-…` (OpenAI) | yes | yes | **no** | **no** |
| 5 | `xoxb-…` (Slack) | yes | yes | **no** | **no** |
| 6 | `eyJ….….…` (JWT) | yes | yes | **no** | **no** |
| 7 | PEM private key | yes | yes | **no** | **no** |
| 8 | env line `LINEAR_API_KEY=` | yes | yes | **no** | **no** |
| 9 | env line `AWS_SECRET_ACCESS_KEY=` | yes | yes | **no** | **no** |
| 10 | env line `password=` | yes | yes | **no** | **no** |
| 11 | env line `export MY_TOKEN=` | yes | yes | **no** | **no** |
| 12 | `Authorization: Bearer …` | yes | yes | **no** | **no** |
| 13 | `lin_api_…` (Linear) | **no** | yes | **no** | **no** |
| 14 | `glpat-…` (GitLab) | **no** | **no** | **no** | **no** |
| 15 | bare 40-char base64/hex | **no** | **no** | **no** | **no** |
| 16 | `npm_…` | **no** | **no** | **no** | **no** |
| 17 | `AIza…` (Google) | **no** | yes | **no** | **no** |

## Reading the table

- **recall** (from `../redaction-cases.md`): a best-effort redactor at capture;
  misses 6.
- **deja-vu**: redacts at ingest and additionally catches `AIza…` and the
  `lin_api_…` shape (as generic entropy); misses 4 (`github_pat_`, `glpat-`,
  `npm_`, bare base64), and those misses are searchable in the index.
- **remem**: no capture-time redaction at all — the raw archive held all 17
  values verbatim, and `remem raw search` returns them. Its only redaction sits
  in the LLM-distillation path, which the isolation rule forbade evaluating.
- **claudemem**: `note add` and `session save` write all 17 values verbatim into
  the markdown source of truth. Only its non-mutating `hook event` candidate
  classifier redacts, and that path never writes.

The shared hard cases are `github_pat_…`, `glpat-…`, `npm_…` and a bare
40-char base64/hex blob; no alternative catches all four. `deja-vu` catches the
most (13/17), then `recall` (11/17).
