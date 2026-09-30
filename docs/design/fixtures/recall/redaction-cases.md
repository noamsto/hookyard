# Recall redaction test cases (all values are fake)

Recall's `scripts/redact.py` was exercised directly, and again end-to-end by
feeding a synthetic Claude Code transcript through `capture.py` and
`make_context.py`, then reading `.recall/history.md` and `.recall/context.md`.
No real credential was ever used; every value below is a placeholder.

Patterns it ships (`_PATTERNS` + `_ENV_LINE`): `sk-…`, `AKIA…`, `gh[pousr]_…`,
`xox[baprs]-…`, `authorization|bearer` values, `eyJ….….…`, PEM private keys,
and `^…(SECRET|TOKEN|PASSWORD|PASSWD|API_KEY|ACCESS_KEY|PRIVATE_KEY)…=value`
assignment lines.

| case | input (fake) | redacted? |
| --- | --- | --- |
| AWS access key id | `AKIAFAKEFAKEFAKEFAKE` | yes |
| GitHub classic PAT | `ghp_FAKEFAKEFAKEFAKEFAKE1234567890` | yes |
| GitHub fine-grained PAT | `github_pat_FAKE11FAKE22FAKE33` | **no** |
| OpenAI key | `sk-FAKEFAKEFAKEFAKEFAKE1234` | yes |
| Slack token | `xoxb-FAKE-1234567890-FAKEFAKE` | yes |
| JWT | `eyJFAKEheaderABC.FAKEpayloadABC.FAKEsig123456` | yes |
| PEM private key | `-----BEGIN RSA PRIVATE KEY-----…` | yes |
| env line, `LINEAR_API_KEY=` | `LINEAR_API_KEY=fakeLinearValue123` | yes |
| env line, `AWS_SECRET_ACCESS_KEY=` | `AWS_SECRET_ACCESS_KEY=fakeAwsSecretValue456` | yes |
| env line, `password=` | `password=FAKEpass123` | yes |
| env line, `export MY_TOKEN=` | `export MY_TOKEN=fakeTokenValue999` | yes |
| Authorization bearer | `Authorization: Bearer FAKEbearer1234567890` | yes |
| Linear API token | `lin_api_FAKELINEARTOKEN1234567890` | **no** |
| GitLab PAT | `glpat-FAKE111111111111` | **no** |
| bare 40-char base64 | `ZmFrZVNlY3JldEZha2VTZWNyZXRGYWtlU2VjcmV0` | **no** |
| npm token | `npm_FAKEFAKEFAKEFAKEFAKEFAKE1234` | **no** |
| Google API key | `AIzaFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE` | **no** |

The `_ENV_LINE` pattern is anchored at line start, so a secret-shaped assignment
embedded mid-line (`… and an env line LINEAR_API_KEY=fakeLinearValue123 and …`)
survives — the same value on its own line is caught.
