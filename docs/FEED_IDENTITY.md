# Feed identity

An installation can have its feed messages posted under its own name and avatar instead of the
Champion bot's.

Off by default. Code: `internal/discord/feed_identity.go`. Setting:
`installation_feature_settings.feed_identity_*`.

## What it is, and is not

Discord only lets a message carry a custom name and avatar when it is sent through a **channel
webhook**. So with an identity enabled, a server's feed messages go through a webhook named
`Champion Feed` that the bot creates (once) in each feed channel and then reuses.

Covered: the kill feed, death feed, hit feed, connections feed, PvE feed and build feed of that
server.

Not covered: slash-command replies, panels, leaderboards, announcements, DMs. Those are
interactions or bot-owned messages and stay the Champion bot. This is a feed identity, not a
separate bot per customer.

## It never costs a message

The feed does not depend on the webhook. Each of these sends the message as the bot instead:

- no identity enabled for the server, or the lookup failed;
- the bot lacks **Manage Webhooks** in the channel (the channel is then left alone for ten minutes
  before trying again);
- the webhook was deleted, or Discord rejected the send - the webhook is looked up afresh for the
  next message;
- the message has attachments, components or is a reply (a webhook send here carries embeds and text only).

The rotating feeds delete their previous batch each cycle. Messages posted through the webhook are
deleted through the webhook too, so cleanup needs no extra permission.

Webhook sends always carry an empty allowed-mentions list: an identity can never ping.

## Settings

`PUT .../admin/features/feed-identity` - capability `FEED_IDENTITY_MANAGE` (**Owner**):

```json
{"enabled": true, "name": "Deadzone Feed", "avatarUrl": "https://cdn.example/deadzone.png"}
```

Validation follows Discord's webhook rules: a name of 1-80 characters that does not contain
"discord" or "clyde", `@`, `#`, `:` or a backtick; `avatarUrl`, if given, must be `https` and at
most 512 characters. The change is audited (`FEED_IDENTITY_UPDATED`) and applies to the next
message (the one-minute identity cache is dropped on save).

## Requirements

The bot needs **Manage Webhooks** in each feed channel. Without it nothing breaks; the feed keeps
posting as the bot and `component=feed_identity event=webhook_unavailable` is logged.
