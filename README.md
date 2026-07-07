# puppet-leonardo

A small Go service that drives [Leonardo.ai](https://app.leonardo.ai) with a real
browser (via [playwright-go](https://github.com/playwright-community/playwright-go))
and exposes **image generation** over:

- a **REST API** (`/api/generate`),
- a simple **web UI** (`GET /`), and
- an **MCP endpoint** (`/mcp`) exposing a `generate_image_with_leonardo` tool for
  LLM agents such as Claude Code.

All three can optionally be published on a public HTTPS URL through an
[https-tunnel](https://github.com/chriswirz/https-tunnel) server (see [Tunnel](#tunnel)).

Given a text prompt it signs in to Leonardo, generates an image, and returns the
URL of the generated image on Leonardo's CDN.

## Requirements

- Go 1.25+
- Google Chrome (recommended) - the app uses the real Chrome channel to reduce
  automation fingerprints; it falls back to Playwright's bundled Chromium.
- A Leonardo.ai account.

## Build & run

```sh
# Windows
./build.ps1
./puppet-leonardo.exe

# Linux (build the binary directly)
go build -o puppet-leonardo .
./puppet-leonardo
```

On first run Playwright downloads the browser it needs. The server listens on the
configured port (default `8080`).

## Configuration

Copy `config.example.json` to `config.json` and fill in your credentials:

| Key                     | Type   | Default    | Description                                                                                   |
| ----------------------- | ------ | ---------- | --------------------------------------------------------------------------------------------- |
| `port`                  | int    | `8080`     | HTTP listen port.                                                                             |
| `headless`              | bool   | `false`    | Run the browser headless. Use `false` if you need to solve the human-verification check.      |
| `default_size`          | string | `16:9`     | Aspect ratio when a request omits `size`. One of `2:3`, `1:1`, `16:9`.                         |
| `profile_dir`           | string | `profile`  | Persistent browser profile directory (launch mode).                                           |
| `step_delay_ms`         | int    | `0`        | Delay inserted between browser steps (helps pages settle).                                     |
| `debug`                 | bool   | `false`    | Log each browser step (`[step] ...`).                                                          |
| `cdp_url`               | string | `""`       | Attach to an already-running Chrome over CDP instead of launching one (see below).            |
| `tasks_file`            | string | `tasks.json` | JSON file holding the task dictionary (see [Tasks](#tasks)); persists across restarts.      |
| `queue_depth`           | int    | `100`      | How many generations may wait behind the running one. Beyond that a request gets `429`; `0` allows no waiting. |
| `clear_dir_on_startup`  | string | `""`       | Directory deleted on startup (e.g. `chrome-cdp`) so each run starts from a clean profile.      |
| `leonardo.email`        | string | -          | Leonardo account email (required).                                                            |
| `leonardo.password`     | string | -          | Leonardo account password (required).                                                         |
| `mcp.instructions`      | string | built in   | Text the MCP server gives clients about itself.                                               |
| `mcp.auth_token`        | string | `""`       | If set, every `/mcp` request must send `Authorization: Bearer <token>`.                        |
| `mcp.allowed_origins`   | array  | `["http://127.0.0.1", "http://localhost"]` | Browser `Origin`s allowed to call `/mcp` (DNS-rebinding defence). No port = any port; `"*"` = any. Requests with no `Origin` are always allowed. |
| `mcp.allowed_headers`   | array  | `[]`       | Headers a CORS preflight is told it may send. Empty echoes the request; `["*"]` allows any.     |
| `mcp.allow_private_network` | bool | `true`   | Answer Chrome's Private Network Access preflight.                                             |
| `mcp.legacy_compatibility`  | bool | `true`   | Also serve the initialize/session-based MCP revisions (2025-03-26 – 2025-11-25).               |
| `mcp.session_timeout_seconds` | int | `7200`  | How long an idle legacy MCP session is kept.                                                  |
| `mcp.generate_wait_seconds` | int | `45`      | How long `generate_image_with_leonardo` waits for the image before returning the task as `running` (a call can override it with `wait_seconds`). Negative waits for completion. |
| `tunnel.*`              | -      | -          | Expose the service on a public URL; see [Tunnel](#tunnel).                                    |

> **Security:** `config.json` holds your Leonardo password in plaintext. Keep it
> out of version control (add it to `.gitignore`) and commit only
> `config.example.json`.

### Getting past Cloudflare "verify you are human" (CDP mode)

Cloudflare Turnstile detects automated browsers. The reliable workaround is to
attach to your own already-running Chrome:

1. Start Chrome with remote debugging and a dedicated profile:
   ```sh
   # Windows
   "C:\Program Files\Google\Chrome\Application\chrome.exe" --remote-debugging-port=9222 --user-data-dir="D:\Source\puppet-leonardo\chrome-cdp"

   # Linux
   google-chrome --remote-debugging-port=9222 --user-data-dir=/tmp/chrome-cdp
   ```
   `--user-data-dir` is essential - without it Chrome routes to an existing
   instance and never opens the port.
2. Sign in to Leonardo once by hand in that window (solve the human check).
3. Set `"cdp_url": "http://localhost:9222"` in `config.json`.
4. Run `puppet-leonardo`. It attaches to that Chrome and reuses the session.

## REST API

### `GET /`
Returns a minimal HTML page for entering a prompt and size.

### `POST /api/generate`
Body: JSON `{ "prompt": "...", "size": "16:9", "wait_seconds": 30 }`. `size` is
optional (one of `2:3`, `1:1`, `16:9`; defaults to `default_size`).
`wait_seconds` is optional: omitted or `0` holds the request until the image is
ready (as before); a positive value returns `202` with the task id if the image
is not ready by then.

Response: `{ "task_id": "<sha256 of prompt>", "url": "https://cdn.leonardo.ai/users/.../generations/.../...jpg" }`

Status codes: `200` success · `202` not finished after `wait_seconds` - the
body is the task (as `GET /api/tasks/{task_id}` returns it, `status` `queued`
or `running`); poll that URL · `400` bad request · `429` the queue is full
(`queue_depth` tasks already waiting; the body carries this request's `task_id`)
· `500` generation error (with `task_id`).

The request is queued and the generation runs in the background, so it
finishes and is recorded even if the caller stops waiting or disconnects.
Sending a prompt that is already queued or running joins that task - it does
not queue a second generation.

`GET /api/generate?prompt=...&size=...&wait_seconds=...` is also supported for quick testing.

### `GET /api/tasks`
Returns the task dictionary - every task, including the one in progress, keyed
by task id. This is exactly the contents of `tasks_file`:

```json
{
  "3f2c...e9a1": {
    "status": "completed",
    "prompt": "a red fox in a snowy forest, watercolor",
    "uri": "https://cdn.leonardo.ai/users/.../generations/.../...jpg",
    "size": "16:9",
    "requested_at": "2026-10-04T14:02:11Z",
    "started_at": "2026-10-04T14:02:11Z",
    "submitted_at": "2026-10-04T14:02:39Z",
    "finished_at": "2026-10-04T14:03:20Z",
    "attempts": 1
  },
  "8b07...41c2": {
    "status": "running",
    "prompt": "a lighthouse at dusk, linocut",
    "size": "2:3",
    "requested_at": "2026-10-04T14:05:02Z",
    "started_at": "2026-10-04T14:05:02Z"
  },
  "c41e...07fd": {
    "status": "queued",
    "prompt": "a paper boat on a puddle, watercolor",
    "size": "16:9",
    "requested_at": "2026-10-04T14:05:20Z"
  }
}
```

### `GET /api/tasks/{task_id}`
Returns one task - its dictionary entry plus its id:

```json
{
  "task_id": "3f2c...e9a1",
  "status": "completed",
  "prompt": "a red fox in a snowy forest, watercolor",
  "uri": "https://cdn.leonardo.ai/users/.../generations/.../...jpg",
  "size": "16:9",
  "requested_at": "2026-10-04T14:02:11Z",
  "started_at": "2026-10-04T14:02:11Z",
  "submitted_at": "2026-10-04T14:02:39Z",
  "finished_at": "2026-10-04T14:03:20Z",
  "attempts": 1
}
```

`status` is `queued` while the task waits its turn (with `queue_position`,
1 = next; this is only in the lookup, never in the file), `running` while it is
being generated, `completed` once the image is ready, and `failed` if its last
run ended in an error (with `error`). While a task is queued or running, `uri`
is absent, or the previous image if the same prompt was generated before. An
unknown id returns `404`.

Timestamps (UTC, RFC 3339) show where a task is:

| Field          | Set when                                                                                   |
| -------------- | ------------------------------------------------------------------------------------------ |
| `requested_at` | the request was accepted and queued.                                                        |
| `started_at`   | the task left the queue and began - signing in to Leonardo first.                          |
| `submitted_at` | the prompt was submitted to Leonardo (the Generate click), after signing in. Updated on a retry. |
| `finished_at`  | the task completed or failed.                                                              |
| `attempts`     | number of submissions; a failed first attempt is retried once after a fresh sign-in.       |

A `queued` task has no `started_at`. A `running` task with no `submitted_at` is
still signing in (which can include waiting up to 3 minutes for the
human-verification check). One with `submitted_at` well past a typical
generation time (~40 s) has likely stalled; it fails on its own after 3 minutes
without an image.

### Tasks

Generations run **one at a time from a queue**, oldest first. Up to
`queue_depth` (default `100`) may wait behind the running one; a request beyond
that is rejected at once (HTTP `429` / MCP tool error) and not recorded.

A request for a prompt that is **already queued or running joins that task**
instead of adding another, and gets the same task id back - so a client that
retries, or several clients asking for the same image, cost one generation.
(A prompt whose task already finished is generated again.) When a request joins
a queued task, the size of the first request is the one used.

Each request's **task id** is the SHA-256 hex digest of its prompt (surrounding
whitespace trimmed), so the same prompt always has the same id and a caller can
compute it up front. Every task is recorded as
`task id -> { "status", "prompt", "size", "uri", "error", "requested_at", "started_at", "submitted_at", "finished_at", "attempts" }`
and written to `tasks_file` (default `tasks.json`) when it is queued (`queued`),
starts (`running`), has its prompt submitted to Leonardo, and completes
(`completed`, with `uri`) or fails (`failed`, with `error`), so the file always
shows the queue and the generation in progress, and survives restarts. The
queue itself does not: a task still `queued` or `running` when the service
starts was cut off by a stop or crash and is marked `failed` ("interrupted") -
request it again. Entries from before statuses existed load as
`completed`. A later generation with the same prompt replaces the earlier entry.
If a client times out while waiting, the generation still finishes;
`GET /api/tasks/{task_id}` (or `get_image_task`) shows its progress.

### Sample calls

**Linux / macOS (bash):**

```sh
curl -s -X POST http://localhost:8080/api/generate \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"a red fox in a snowy forest, watercolor","size":"16:9"}'
```

**Windows (PowerShell) - use `curl.exe` to avoid the `Invoke-WebRequest` alias:**

```powershell
curl.exe -s -X POST http://localhost:8080/api/generate `
  -H "Content-Type: application/json" `
  -d '{\"prompt\":\"a red fox in a snowy forest, watercolor\",\"size\":\"16:9\"}'
```

**Windows (PowerShell native):**

```powershell
Invoke-RestMethod -Uri http://localhost:8080/api/generate -Method Post `
  -ContentType 'application/json' `
  -Body '{"prompt":"a red fox in a snowy forest, watercolor","size":"16:9"}'
```

## MCP endpoint

`/mcp` is an MCP server on the Streamable HTTP transport (JSON-RPC 2.0), served
by the same HTTP listener as the REST API. It exposes two tools:

**`generate_image_with_leonardo`** - same as `POST /api/generate`:

- `prompt` (string, required)
- `size` (string, optional - `2:3`, `1:1`, or `16:9` - default)
- `wait_seconds` (integer, optional) - how long to wait for the image; default
  `mcp.generate_wait_seconds` (45). `0` returns at once.

Generation usually takes longer than the wait, which keeps the call under
client tool-call timeouts. If the image is ready in time, the first text content
block is the image URL and the second is `task_id: <id>`; `structuredContent`
is `{"task_id", "status": "completed", "url"}`. Otherwise the call returns
(not as an error) with the task as `structuredContent` (`status` `queued`, with
`queue_position`, or `running`) and text telling the model to poll
`get_image_task` rather than call again. Failures (bad arguments, a full queue,
generation errors within the wait) come back with `isError: true`.

**`get_image_tasks`** - no arguments; same as `GET /api/tasks`. Returns the
task dictionary as JSON text and as `structuredContent`.

**`get_image_task`** - `task_id` (string, required); same as
`GET /api/tasks/{task_id}`. Returns the task's `task_id`, `status`, `prompt`
and `uri`; an unknown id is a tool error.

Protocol support mirrors puppeteer-mcp-go:

- **2026-07-28** (stateless): each request declares its version in
  `params._meta["io.modelcontextprotocol/protocolVersion"]` and mirrors it in the
  `MCP-Protocol-Version`, `Mcp-Method` (and for `tools/call`, `Mcp-Name`)
  headers. `server/discover` lists versions and capabilities;
  `subscriptions/listen` opens a keep-alive SSE stream.
- **2025-03-26 / 2025-06-18 / 2025-11-25** (legacy, on by default via
  `mcp.legacy_compatibility`): `initialize` mints an `Mcp-Session-Id`;
  `GET /mcp` opens the keep-alive SSE stream and `DELETE /mcp` ends the session.
  A request with no session and no version header is treated as 2025-03-26, so
  the bare `curl` calls below work without a handshake.

Requests carrying a browser `Origin` not in `mcp.allowed_origins` are refused
with `403`, and when `mcp.auth_token` is set every request needs
`Authorization: Bearer <token>`.

Register it with Claude Code:

```sh
claude mcp add --transport http leonardo http://localhost:8080/mcp
# with a token:
claude mcp add --transport http leonardo http://localhost:8080/mcp --header "Authorization: Bearer <token>"
```

List the tool:

```sh
curl -s -X POST http://localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

Call the tool:

```sh
curl -s -X POST http://localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"generate_image_with_leonardo","arguments":{"prompt":"a serene mountain lake at dawn","size":"16:9"}}}'
```

## Tunnel

The service can be published on a public HTTPS URL through an
[https-tunnel](https://github.com/chriswirz/https-tunnel) server, so a hosted
client (e.g. a claude.ai connector) can reach it without opening a port. The
tunnel client serves the same handler in process - `/`, `/api/generate` and
`/mcp` - and reconnects on its own.

```json
"tunnel": {
  "enabled": true,
  "server_url": "https://tunnel.example.com",
  "api_key": "",
  "api_key_env": "TUNNEL_API_KEY",
  "subdomain": "leonardo",
  "session_file": ".puppet-leonardo-tunnel-session",
  "only": false
}
```

| Key            | Description                                                                                   |
| -------------- | --------------------------------------------------------------------------------------------- |
| `enabled`      | Turn the tunnel on.                                                                           |
| `server_url`   | The tunnel server.                                                                            |
| `api_key`      | Key for that server; leave empty to read the variable named by `api_key_env` (default `TUNNEL_API_KEY`). |
| `subdomain`    | Requested label. Granted when free, otherwise a random one is issued - read the logged URL.   |
| `session_id`   | Resume a specific session (and its URL). Normally left empty.                                 |
| `session_file` | Where the issued session id is saved so a restart reclaims the same URL. Empty = written into `config.json`. |
| `only`         | Serve through the tunnel alone, binding no local port.                                        |
| `client_info`  | Client description sent to the server (default `puppet-leonardo 1.0.0`).                      |

The same settings are available as flags, which override the config:

```sh
./puppet-leonardo -tunnel https://tunnel.example.com -tunnel-key "$TUNNEL_API_KEY" \
  -tunnel-subdomain leonardo -tunnel-session-file .puppet-leonardo-tunnel-session
./puppet-leonardo -tunnel-only   # tunnel without a local listener
```

On connect the server logs `tunnel up: https://<label>.tunnel.example.com (MCP endpoint: .../mcp)`.

> **Security:** the tunnel is public and every generation spends your Leonardo
> tokens. Set `mcp.auth_token`. Note that only `/mcp` checks the token - `/` and
> `/api/generate` are reachable by anyone with the URL while the tunnel is up.

---

## For an AI model / agent (e.g. Claude Code)

**How to use this image generation API**

You can generate images with Leonardo.ai through this local service. There are
two ways to call it; pick whichever your runtime supports.

**1. Plain HTTP (simplest).** Send a POST to `http://localhost:8087/api/generate`
with a JSON body `{"prompt": "<describe the image>", "size": "<2:3|1:1|16:9>"}`.
`size` is optional and defaults to `16:9`. The response is
`{"task_id": "<sha256 of prompt>", "url": "https://cdn.leonardo.ai/.../<file>.jpg"}` -
use that URL as the generated image. Example:

```sh
curl -s -X POST http://localhost:8080/api/generate \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"A management blog cover art for \"Developing a Work Breakdown Structure\" (do not indicate in the image that is is a blog)","size":"16:9"}'
```
And the expected response should be 
```
{
    "task_id": "<sha256 of the prompt>",
    "url": "https://cdn.leonardo.ai/users/<user id>/generations/<generation id>/gpt-image-2_Developing_a_Work_Breakdown_Structure_stylized_fa-0.jpg"
}
```
**2. MCP tool.** If you speak MCP, the endpoint `http://localhost:8080/mcp`
exposes the tool `generate_image_with_leonardo(prompt, size?)`. Call it via
`tools/call`. If the image was ready within the call's wait, the first text
content block is the image URL; otherwise the result says the task is still
running and gives its `task_id` - poll `get_image_task(task_id)` every 20-30
seconds until `status` is `completed` and use its `uri`.

**Notes for the caller:**
- Generation takes one to a few minutes, plus time waiting in the queue. Over
  MCP, expect a `queued` or `running` result and poll `get_image_task`. Over
  REST, either allow a long timeout or send `wait_seconds` and poll
  `GET /api/tasks/{task_id}` on a `202`.
- Requests are queued and run one at a time. Don't resend a prompt to speed it
  up: a prompt that is already queued or running joins its existing task. Only
  a full queue returns HTTP `429` (or an MCP tool error) - retry later.
- If your call times out anyway, the generation still completes. Its task id is
  the SHA-256 hex of your prompt; check it with `GET /api/tasks/{task_id}` (or
  the `get_image_task` tool) until `status` is `completed` (use the `uri`) or
  `failed` (read the `error`). Avoid polling `get_image_tasks`: it returns the
  whole history every time.
- Put the full scene description in `prompt`; keep it self-contained. Choose
  `size` by intended use: `16:9` for banners/covers, `1:1` for avatars/thumbnails,
  `2:3` for posters/portraits.
- The returned URL is a direct link to the `.jpg` on Leonardo's CDN; you can embed
  or download it directly.
