# Lemmy Reply Bot

This project is a simple bot that replies to comments on Lemmy. Every 10 seconds, it fetches the 200 newest comments from your configure Lemmy instance, and sees if they match any regex configured in the config file. If it finds one that does, it replies with the message corresponding to that regex.

### Configuration

This repo contains a file called `lemmy-reply-bot.example.toml`. This is an example config file. Copy it to `lemmy-reply-bot.toml` and edit it to fit your needs. The config contains your password, so its permissions must be set to 600 or the bot will refuse to start.

If any regular expressions configured in the file also match the reply messages, the bot will refuse to start because this may cause an infinite loop.

### Debugging

In order to enable debug log messages, set `LEMMY_REPLY_BOT_DEBUG=1`.