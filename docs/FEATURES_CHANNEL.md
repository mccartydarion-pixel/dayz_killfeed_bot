# Features channel

`/features` turns a read-only Discord channel on or off that lists Champion's newest features and
how players use them.

## Use

- Only members with Administrator or Manage Server can run it.
- First run: creates `📢・champion-features` in the Champion category (or at the top level if that
  category is gone) and posts one card per feature.
- Next run: deletes that channel.
- If the channel was deleted by hand, the next run creates a new one.

## Rules

- Everyone can read the channel; nobody but the bot can post, react or open threads in it.
- The bot records the channel it created (`guilds.features_channel_id`, migration
  `0117_guild_features_channel`) and only ever deletes that channel, and only when it is still in
  the same Discord server.
- All or nothing: if the setting cannot be saved or the guide cannot be posted, the new channel is
  deleted again and nothing is left behind.
- If Discord cannot be reached when checking the existing channel, nothing changes.

## Content

The cards are `featuresGuideEmbeds` in `internal/discord/features_command.go`. Only features that
are live for everyone belong there, nothing still in testing. After changing them, staff run
`/features` twice to repost the guide.
