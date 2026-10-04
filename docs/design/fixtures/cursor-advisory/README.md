# Cursor advisory channel (issue #140)

Evidence for where Cursor can carry a standalone hookyard advisory, i.e.
advice that does not ride a rendered `permission`. See
`internal/verdict/capability.go` (`HasAdvisorySlot`) and
`docs/design/hookyard.md` §7 for how it is used.

## Result

`sessionStart` and `postToolUse` take a top-level `additional_context`
string, and hookyard renders Cursor's standalone advice there:

```json
{"additional_context":"<advice>"}
```

Cursor's docs and its shipped `cursor-agent` bundle agree on those two
events. They disagree on `beforeSubmitPrompt` and `preToolUse`, so those
stay out of Cursor's advisory set:

| native event | [Cursor hooks docs](https://cursor.com/docs/agent/hooks) | `cursor-agent` 2026.10.01-e373342 bundle | hookyard |
| --- | --- | --- | --- |
| `sessionStart` | output `{env, additional_context}`; `additional_context` is "additional context to the conversation's initial system context" | validator accepts `env`, `additional_context`, `continue`, `user_message`; the CLI resolves `additional_context` into a promise that every request attaches as `RequestContext.hooksAdditionalContext` | advisory slot |
| `postToolUse` | output `{updated_mcp_tool_output, additional_context}`; "extra context injected into the conversation after the tool result" | validator accepts `additional_context`; `hooks-carriers` pushes a `HookAdditionalContext{hookEventName, content}` carrier onto the tool message | advisory slot |
| `beforeSubmitPrompt` | output `{continue, user_message}` only | validator and `hooks-carriers` accept `additional_context` | **no** advisory slot: code-read only, contradicted by the docs |
| `preToolUse` | output `{permission, user_message, agent_message, updated_input}` | validator and `hooks-carriers` accept `additional_context`, on a deny too | decision slot only: advice rides `user_message` beside a permission |

The grade is **DOC + code-reading, not LOCAL**. `cursor-agent status` on this
host prints `Not logged in`, and `cursor-agent` runs no prompt without an
interactive login — the same blocker `../cursor-plugin-root/README.md`
records for #45. Whether Cursor's backend actually puts
`hooksAdditionalContext` into the model's context is server-side; the bundle
shows only that the client sends it.

## Size cap

`hooks-carriers` lists the steps that support `additional_context`:

```
HOOK_STEPS_SUPPORTING_ADDITIONAL_CONTEXT =
  {sessionStart, beforeSubmitPrompt, preToolUse, postToolUse, postToolUseFailure}
```

It trims the value (`String.prototype.trim`), drops an empty one, and throws
`HookAdditionalContextTooLargeError` above 10000 characters of JS string
length — UTF-16 code units, not bytes or runes. The `postToolUse` caller
catches that and drops the carrier with a warning. hookyard still prints
over-cap advice, but records it undelivered (`cursorAdditionalContextMax`
in `internal/verdict/render.go`), trimming and measuring as JS does: U+FEFF is
trimmed and U+0085 is not, unlike Go's `strings.TrimSpace`. The CLI's own
`sessionStart` path was not seen going through the capped helper, so for
`sessionStart` that record may under-claim. It never over-claims.

## Where the code is

Bundle: `/nix/store/cm7rnkjd0nppn4fp3i87jyn0c8dsnqj8-cursor-agent-2026.10.01-e373342/share/cursor-agent/`
(content-addressed, so the path is a stable citation for this version).
`index.js` carries the output validators, `190.index.js` the
`hooks-carriers` module and the `RequestContext` wrapper, and `7000.index.js`
the CLI's `sessionStart` and `beforeSubmitPrompt` calls. The bundle is
minified and proprietary, with no public source to permalink. Chunk numbers
change between versions, so search by string:

```
cd <bundle dir>
rg -o -N '.{0,250}additional_context.{0,250}' index.js 190.index.js 7000.index.js
rg -o -N '"\.\./hooks-carriers/dist/index\.js"\(e,t,o\).{0,1800}' 190.index.js
rg -o -N '.{0,600}sessionStart additional_context received.{0,300}' 7000.index.js
rg -o -N 'r=await this\.hooksAdditionalContextPromise.{0,900}' 190.index.js
```

The Cursor hooks page was read on 2026-10-04.

## Probe recipe (LOCAL grade, needs a human login)

This upgrades the code-reading result to LOCAL. Run it from a scratch
directory, never from this repo. `env -u` keeps a dispatcher worker's bus
identity out of `~/.cursor/hooks.json`'s own hooks.

```
cursor-agent login   # interactive, once
mkdir -p /tmp/cursor-advisory-probe/.cursor && cd /tmp/cursor-advisory-probe
cat > .cursor/hooks.json <<'EOF'
{"version":1,"hooks":{"sessionStart":[{"command":"printf '%s' '{\"additional_context\":\"The probe codeword is PERIWINKLE-42.\"}'"}]}}
EOF
env -u CREW_WORKER_ID -u CREW_ID cursor-agent -p --trust --output-format text \
  "What is the probe codeword in your context? Reply with the codeword only."
```

`PERIWINKLE-42` in the reply means `sessionStart` delivery is confirmed. To
check the `beforeSubmitPrompt` and `preToolUse` claims the docs omit, repeat
with the hook on that event, and a tool call for `preToolUse`.
