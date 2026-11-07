# Source note

The plugin's WorkBuddy protocol support is ported from WorkBuddy's own clients and
from the MIT-licensed `workbuddy` package in `magpie-community/plugins`. The Go
implementation in `go/` is independent code, but its wire behaviour follows those
sources:

- **Sign-in flow**: the browser page WorkBuddy's desktop opens
  (`POST /v2/plugin/auth/state`, `GET /v2/plugin/auth/token`,
  `GET /v2/plugin/login/account`), the `version` and `loginSessionId` the desktop
  adds to the page, and the pending codes `11217` and `12151`.
- **Token refresh**: `POST /v2/plugin/auth/token/refresh` with the refresh token in
  `X-Refresh-Token`, `X-Auth-Refresh-Source: plugin`, the account's `X-Domain` and
  the enterprise headers WorkBuddy's own client sends.
- **Request headers**: the desktop's `User-Agent`
  (`WorkBuddy/<app> <appName>/<app> CLI/<cli>`), the client attribution its usage
  list reads (`X-Product`, `X-IDE-Type`, `X-IDE-Name`, `X-IDE-Version`,
  `X-Agent-Purpose`, `X-Agent-Intent`, `X-Requested-With`), the agent and runtime
  headers, the `X-Stainless-*` set of the bundled OpenAI SDK, and the four
  conversation headers.
- **Request shaping**: `max_tokens` instead of `max_completion_tokens`, `system`
  instead of `developer`, `reasoning` instead of `reasoning_content`, the default
  system message, and the reworded lines WorkBuddy refuses as an unapproved channel
  (error `11128`).
- **Model listing**: the `cli` agent's models in `/v3/config`, the effort and image
  fields that describe them, and the per-build differences (WorkBuddy AI's text-only
  GLM models, the China build's Messages-only Claude models).
- **Billing**: `/billing/meter/get-user-resource-summary` and its credit groups and
  packages, `/billing/meter/get-enterprise-user-usage` for a team account, and the
  UTC+8 cycle timestamps both send.

WorkBuddy, CodeBuddy and their marks belong to their owner. This project is not
affiliated with or endorsed by them.

## Marks

`assets/workbuddy.svg` is WorkBuddy's own mark, the artwork its clients and site
publish. It is the plugin logo and the China build's sign-in card.
`assets/codebuddy-color.svg` is CodeBuddy's mark, which the international build's
assets are served from; it is the WorkBuddy AI card. Both are used only to identify
the service a credential signs in to, and this project publishes no mark of its own.

`go/assets/` holds the copies the plugin embeds and serves, and they must stay
byte-identical to the files under `assets/`; the test suite compares them.
