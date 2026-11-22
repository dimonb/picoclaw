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
        "group_mode": "smart",
        "disable_inbound_reaction": false,
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
| settings.group_mode    | string | No       | `smart` (default): the bot reads every post in its channels and decides itself whether to answer. `mention`: it only sees @mentions, follow-ups in its threads and `group_trigger` prefixes |
| settings.disable_inbound_reaction | bool | No | `true` turns off the automatic 👀 on posts addressed to the bot (same key as Telegram). Default `false`. Reactions the bot chooses itself are not affected |
| settings.streaming     | object | No       | `{"enabled": true}` streams partial replies by editing a post in place (`throttle_seconds`, `min_growth_chars` optional) |
| allow_from             | array  | No       | Mattermost user IDs (or `mattermost:<user_id>`) allowed to talk to the bot; empty means everyone    |
| typing.enabled         | bool   | No       | Show the "is typing…" indicator while the agent works                                                |
| placeholder.enabled    | bool   | No       | Post a placeholder ("Thinking…") that is edited into the final answer                                |
| group_trigger.prefixes | array  | No       | Optional extra trigger for channel posts without an @mention                                         |

The token can also be supplied via `PICOCLAW_CHANNELS_MATTERMOST_BOT_TOKEN` (and the URL via `PICOCLAW_CHANNELS_MATTERMOST_URL`).

## Behavior

- **Direct messages** are always answered.
- **Channel and group messages** depend on `group_mode`:
  - **`smart` (default):** the bot reads every post in channels it is a member of and behaves like a colleague in the chat. It answers when it is addressed, when the topic concerns it, or when it has something genuinely useful to add. Otherwise it stays silent: the agent answers `NO_REPLY`, and nothing is posted.
  - **`mention`:** only @mentions, follow-ups in threads the bot takes part in, and `group_trigger.prefixes` reach the agent. Use this for noisy or expensive channels.
- A follow-up in a thread where the bot has replied counts as addressing the bot, like replying to the bot in Telegram. The @mention is removed from the text before it reaches the agent. `@channel`, `@all` and `@here` alone do not count as a mention.
- The 👀 reaction (unless `disable_inbound_reaction` is set), typing indicator and placeholder appear up front only for posts that address the bot. For other channel posts, a reply (if any) simply appears.
- Several posts sent while the bot is working on a channel are folded into the running turn, which gives one reply instead of one per line. An *addressed* post from another thread gets its own turn and its own threaded reply.
- **One conversation per channel.** A DM is one continuous conversation, and each channel or group is one conversation across all of its threads, so the bot remembers context from other threads in the same channel.
- **Where replies go** is one mechanism for DMs and channels; only the default differs:
  - **DMs mirror the user:** a top-level message gets a top-level reply, and a message inside a thread is answered in that thread.
  - **Channels and groups:** replies go into the thread of the message being answered (under the message itself if it was top-level).
  - **The bot may choose otherwise.** It is told the default for each message and can send its reply with the message tool instead: `reply_to_message_id` puts the reply in that message's thread, and leaving it out posts at the top level. The bot then answers `NO_REPLY`. It is advised to keep short DM exchanges at the top level, and to leave a channel thread only when that really fits better (an announcement, one answer to several people).
  - Placeholders, streamed edits, the typing indicator and files follow the default placement. They are removed if the bot posts elsewhere. If messages arrive in two different threads at once, each is answered in its own thread.
  - The advice can be replaced per workspace with `THREADS_DIRECT.md` / `THREADS_GROUP.md` (defaults in `pkg/agent/group_chat.go`).
- Message IDs shown to the agent (and accepted by the message tool's `reply_to_message_id`) look like `<channel_id>:<post_id>` for top-level posts and `<channel_id>:<root_id>:<post_id>` inside a thread. Sending to a bare channel ID with no reply target creates a top-level post.
- The bot ignores its own posts, system messages (joins, header changes, …) and posts from other bots.
- Replies longer than 16383 characters are split into several posts.
- **Reactions are the bot's choice, as in Telegram.** With the reaction tool it can react to any message by its ID (`<channel_id>:<post_id>` or `<channel_id>:<root_id>:<post_id>`), in DMs and channels. It can also react *instead of* replying (reaction + `NO_REPLY`). Telegram's reaction emoji and common extras are mapped to Mattermost names, and any Mattermost emoji, custom ones included, can be given as `:name:`. Cleaning up after a turn removes only the automatic 👀, never a reaction the bot placed, even 👀.
- **Tool feedback** (`agents.defaults.tool_feedback`) is shown in one post per thread that is edited in place. When the answer arrives, the answer replaces that post, or the post is deleted if there is a placeholder.
- **Editing its own posts.** The bot can fix or delete a post it sent earlier with the `edit_message` tool (`delete: true` removes it). It is told it may fix a mistake or update a status this way instead of sending a new message. IDs come from the message tool's result or from history, where its own replies carry `msgs:#<channel_id>:[<root_id>:]<post_id>`. History shows those IDs only when the `seahorse` context manager is used; streamed replies are covered too.
- **Edited user posts** (`post_edited`): when a user changes a post the bot handled in the last 24 hours, the bot receives the new text as a new message. It has the same message ID and starts with `[edited earlier message; respond only if the change matters]`. The bot decides whether to react; usually it answers `NO_REPLY` for a typo fix. No 👀, typing or placeholder appears for a correction, and the edit stays in history. Changes to link previews or other metadata are ignored, as are edits of older posts. A channel post edited to add an `@mention` of the bot counts as a new request.
- Attachments are downloaded for the agent; files the agent sends are uploaded as attachments.

## Setup

1. As a system admin, enable bot accounts: **System Console → Integrations → Bot Accounts → Enable Bot Account Creation**
2. Create a bot: **Integrations → Bot Accounts → Add Bot Account**, and copy its access token
3. Add the bot to your team and invite it to the channels where it should respond
4. Find your user ID (Profile → ⋮ → Copy user ID) and put it into `allow_from`
5. Fill in `url` and `bot_token`, then run `picoclaw gateway`
