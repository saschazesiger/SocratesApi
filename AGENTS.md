# Working on Socrates API

Socrates API makes self-hosting LLMs easy: one container, a Go server that supervises llama.cpp's `llama-server`
for one active model, a web interface and an OpenAI/Anthropic-compatible API. Read
[docs/memory.md](docs/memory.md) and [docs/development.md](docs/development.md) before changing behaviour.

## Layout

See [docs/development.md](docs/development.md#-layout). The interface's JSON contract is
[docs/internal-api.md](docs/internal-api.md); change both sides together.

## Rules

- **llama.cpp decides the placement.** Socrates asks `llama fit-params`, starts with exactly that result and
  `--fit off`, and only measures afterwards. Do not invent memory formulas where llama.cpp can be asked.
- **Show what is true.** Every figure in the interface is measured or comes from llama.cpp; estimates are labelled.
- **Never lose a request quietly.** The proxy waits for a loading model, and every request ends up in the log.
- **Errors are for the person in front of the interface.** Say what happened and what to do, in plain English.
- **Interface: Z-Dash only.** Components `ds-*`, option classes, `DS.*` helpers as documented in
  <https://github.com/saschazesiger/Z-Dash>. Project CSS is a theme (tokens) plus layout glue with the prefix `so-`.
  No framework, no bundler. `web/vendor/z-dash/` is a verified copy: never edit it.
- **Few dependencies.** Standard library, `golang.org/x/crypto`, `modernc.org/sqlite`.
- Text and documentation are English, with little prose: tables, lists, emojis as in the README. Comments say why.
- **Test, then push to `main` yourself.** Run the checks below (and try the change on a real GPU where it touches
  the engine). Once sure, rebase onto `origin/main`, run the tests again and push to `main`; no pull request needed.
  If not sure, do not push: say what is untested.

## Commands

```sh
go build ./... && go vet ./... && go test ./...
sh web/vendor/z-dash/tools/verify.sh web/vendor/z-dash
node web/vendor/z-dash/tools/check-project.mjs --config web/ds-check.json
node scripts/mock-server.mjs     # the interface on simulated data, http://localhost:8090
```
