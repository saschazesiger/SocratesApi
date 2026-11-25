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
- **On the Z-Dash server.** The listener, the sign-in, the guards, the change notices, the Cloudflare tunnel and
  Z-Dash itself come from `github.com/saschazesiger/Z-Dash/server` (its `docs/server.md`); Socrates registers its
  routes on it and keeps its own database. A fix to any of those goes to Z-Dash, then `go get …@main` here (the
  nightly *Update Z-Dash server* does that). The keys `auth.*` and `cloudflare:*` of the `kv` table are the server's.
- **Interface: Z-Dash only.** Components `ds-*`, option classes, `DS.*` helpers as documented in
  <https://github.com/saschazesiger/Z-Dash>, loaded from the server (`/z-dash/ds.min.css`, `/z-dash/ds.min.js`), with
  its shell (`/z-dash/server/`) for the sign-in, the account and the tunnel and password settings. Project CSS is a
  theme (tokens) plus layout glue with the prefix `so-`. No framework, no bundler. `web/web.go` embeds only
  Socrates' own files; `web/.z-dash/` is a copy of the module's Z-Dash for its tools (gitignored).
- **Few dependencies.** Standard library, `golang.org/x/crypto`, `modernc.org/sqlite`, the Z-Dash server.
- Text and documentation are English, with little prose: tables, lists, emojis as in the README. Comments say why.
- **Test, then push to `main` yourself.** Run the checks below (and try the change on a real GPU where it touches
  the engine). Once sure, rebase onto `origin/main`, run the tests again and push to `main`; no pull request needed.
  If not sure, do not push: say what is untested.

## Commands

```sh
go build ./... && go vet ./... && go test ./...
go mod download github.com/saschazesiger/Z-Dash/server
ZD="$(go list -m -f '{{.Dir}}' github.com/saschazesiger/Z-Dash/server)/zdash/dist"
rm -rf web/.z-dash && cp -R "$ZD" web/.z-dash && chmod -R u+w web/.z-dash   # the module's Z-Dash, for its tools
sh web/.z-dash/tools/verify.sh web/.z-dash
node web/.z-dash/tools/check-project.mjs --config web/ds-check.json
node scripts/mock-server.mjs     # the interface on simulated data, http://localhost:8090
```
