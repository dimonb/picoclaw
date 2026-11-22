> Back to [README](../../../README.md)

# Mattermost

Mattermost is an open-source, self-hostable team chat platform. PicoClaw connects as a Mattermost bot account: it receives events over the Mattermost WebSocket API and replies through the REST API (v4), so no public webhook endpoint is needed.

## Configuration

```json
{
  "channel_list": {
    "mattermost": {
      "enabled": true,
      "type": "mattermost",
      "allow_from": ["8x4kq1wq7jbz5f3n9u6c2m1abc"],
      "typing": { "enabled": true },
      "placeholder": { "enabled": false },
      "settings": {
        "url": "https://mattermost.example.com",
        "bot_token": "YOUR_MATTERMOST_BOT_TOKEN",
        "team": "",
        "bot_username": "",
        "streaming": { "enabled": false }
      }
    }
  }
}
```

| Field                  | Type   | Required | Description                                                                                          |
| ---------------------- | ------ | -------- | ---------------------------------------------------------------------------------------------------- |
| enabled                | bool   | Yes      | Whether to enable the Mattermost channel                                                             |
| settings.url           | string | Yes      | Server base URL, e.g. `https://mattermost.example.com` (the WebSocket URL is derived from it)       |
| settings.bot_token     | string | Yes      | Bot account access token (or a personal access token). Treated as a secret, like Slack's tokens     |
| settings.team          | string | No       | Team name or ID. When set, channel posts from other teams are ignored (DMs are always handled)      |
| settings.bot_username  | string | No       | Overrides the username from `/api/v4/users/me` used to detect `@mentions` in text                   |
| settings.streaming     | object | No       | `{"enabled": true}` streams partial replies by editing a post in place (`throttle_seconds`, `min_growth_chars` optional) |
| allow_from             | array  | No       | Mattermost user IDs (or `mattermost:<user_id>`) allowed to talk to the bot; empty means everyone    |
| typing.enabled         | bool   | No       | Show the "is typing…" indicator while the agent works                                                |
| placeholder.enabled    | bool   | No       | Post a placeholder ("Thinking…") that is edited into the final answer                                |
| group_trigger.prefixes | array  | No       | Optional extra trigger for channel posts without an @mention                                         |

The token can also be supplied via `PICOCLAW_CHANNELS_MATTERMOST_BOT_TOKEN` (and the URL via `PICOCLAW_CHANNELS_MATTERMOST_URL`).

## Behavior

- **Direct messages** are always answered. **Channel and group messages** are answered only when the bot is @mentioned; the mention is removed from the text before it reaches the agent.
- **One conversation per channel.** A DM is one continuous conversation, and each channel or group is one conversation across all of its threads, so the bot remembers context from other threads in the same channel.
- Replies are still posted **in a thread**: under the thread the message belongs to, or under the message itself if it was top-level. Placeholders, streamed edits, the typing indicator and files go to that same thread. If messages arrive in two different threads at once, each is answered in its own thread.
- Message IDs shown to the agent (and accepted by the message tool's `reply_to_message_id`) look like `<channel_id>:<post_id>` for top-level posts and `<channel_id>:<root_id>:<post_id>` inside a thread. Sending to a bare channel ID with no reply target creates a top-level post.
- The bot ignores its own posts, system messages (joins, header changes, …) and posts from other bots.
- Replies longer than 16383 characters are split into several posts.
- Inbound posts receive a 👀 reaction while the agent works. Attachments are downloaded for the agent; files the agent sends are uploaded as attachments.

## Setup

1. As a system admin, enable bot accounts: **System Console → Integrations → Bot Accounts → Enable Bot Account Creation**
2. Create a bot: **Integrations → Bot Accounts → Add Bot Account**, and copy its access token
3. Add the bot to your team and invite it to the channels where it should respond
4. Find your user ID (Profile → ⋮ → Copy user ID) and put it into `allow_from`
5. Fill in `url` and `bot_token`, then run `picoclaw gateway`
