# Telegram Forwarder to HTTP

Telegram bot that forwards `/xxxx` commands to configured HTTP webhooks. Uses bot-token auth only.

My personal usecase was intercepting some particular commands and forward them to n8n or picoclaw without the burden of the whole chat history.

## Setup

0. Build it with `goreleaser build --clean --snapshot --single-target`
1. Copy `config-sample.yaml` in `config.yaml`
2. Set `bot_token`, `enabled_user_ids`, and `webhooks` (`name`, `url`, `matching_string`; optional `header` / `header_value`).
3. Run `tgforwarder -config config.yaml`
4. Send a message to your bot.

## Behavior

- Only messages from listed user IDs are handled.
- Commands like `/abc` route to the webhook whose `matching_string` is `abc`.
- POST body is JSON (user, chat, message, attachments, etc.).

## Dev

Note: I used Cursor and then fixed by hand things that I didn't like: the overall quality of Cursor-generated code lately isn't _that_ high, IMHO.

```bash
go test ./...
golangci-lint run ./...
```

## TODO

- [ ] add some envvar handling (for token and configfile location)
- [ ] finish test implementation with full mocking
- [ ] add Dockerfile
- [ ] add CI