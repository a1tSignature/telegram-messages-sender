# Telegram Messages Sender

**English** · [Русский](README.ru.md)

A local macOS app that publishes recurring messages through a dedicated session of your personal Telegram account. The desktop app owns the sending queue. `tmsctl` controls that same queue over an authenticated loopback connection.

[Build and run](#build-and-run) · [Set up Telegram](#set-up-telegram) · [Publications and controls](#publications-and-controls) · [Platform verification](#platform-verification)

## Build and run

Use an Apple Silicon Mac, Go 1.26.3 as specified in `go.mod`, and the Xcode Command Line Tools (`xcode-select --install` if needed). The package targets macOS 15.0. See [platform verification](#platform-verification) for the versions tested at runtime.

```sh
go mod download
sh ./scripts/package-macos.sh
open dist/telegram-messages-sender.app
./dist/tmsctl status
```

The script builds both arm64 executables with CGO, verifies their Mach-O deployment target and the app's Info.plist, and applies a local ad-hoc signature. It writes the app bundle and standalone CLI to `dist/`. Local package version: `1.0.0`.

## Set up Telegram

1. Obtain your API ID and API hash in **API development tools** at my.telegram.org. [Telegram's API credential instructions](https://core.telegram.org/api/obtaining_api_id)
2. Open **API credentials**, enter the values and choose **Save to Keychain**.
3. Choose **Sign in**, scan the QR code from Telegram's device-linking screen, and enter your Telegram two-step verification password if prompted. The window displays the verified account and connection status.
4. Choose **New publication**. Give it a stable ID such as `announcements`, set an interval such as `20m`, add variants and destinations, then save and choose **Start**.

The authorization session carries the authority of your personal Telegram account. Treat the Keychain items and API hash as secrets. If you lose control of the session, revoke it from Telegram's **Devices** settings and sign in again. SQLite stores publication settings and delivery history in `~/Library/Application Support/telegram-messages-sender/`. The API credentials and session are stored in macOS Keychain.

## Publications and controls

- Use manual text or select a Saved Messages entry containing text, one photo or one video. The app fetches the current source before each new send. Saved Messages forwarding requests omit attribution and retain captions. Live rendering of attribution, captions and formatting is pending.
- Select basic groups or non-forum supergroups where you are an administrator or owner and can publish under your personal profile. Broadcast channels, forum topics, anonymous administrator identities, albums and standalone file attachments are outside the supported scope.
- Repeats are scheduled per publication and chat from confirmed delivery. A new cycle starts with the first destination, then schedules the remaining destinations at independent offsets from that same start. Work missed during an outage is coalesced. Closing the last window stops dispatch and allows a bounded wait for an in-flight attempt.

```sh
./dist/tmsctl status
./dist/tmsctl start announcements
./dist/tmsctl stop announcements
./dist/tmsctl change-interval announcements 30m
./dist/tmsctl re-enable-chat supergroup:123456789
```

Use the publication and chat IDs from `status`. A successful change returns the committed revision as JSON. When the desktop owner is closed, the CLI exits with an error.

| Status | Meaning |
| --- | --- |
| Rights lost | The chat is disabled across publications. Restore the required rights and explicitly re-enable the chat to run a fresh check. |
| Verification unavailable or posting restricted | The app skips the send while it cannot verify permission or the personal sender identity. |
| Source unavailable or variants exhausted | A temporarily unavailable source is retried. A confirmed missing source makes that variant unavailable. The app pauses the publication when no variants remain. |
| Session expired, wrong account or Keychain access failure | Dispatch is blocked. Check Keychain access or sign in with the account bound to the publication settings. |
| Telegram wait | The app defers work for the indicated chat or the entire account until the wait expires. |
| Unknown delivery | Telegram may have received the message. The app keeps the attempt visible and uses subsequent evidence to confirm it. Any later scheduled send is a new attempt. |

## Platform verification

```sh
go test ./...
go vet ./...
go test -race ./internal/control ./internal/dispatch
sh ./scripts/package-macos.sh
xcrun lipo -archs dist/telegram-messages-sender.app/Contents/MacOS/telegram-messages-sender
xcrun vtool -show-build dist/telegram-messages-sender.app/Contents/MacOS/telegram-messages-sender
xcrun vtool -show-build dist/tmsctl
plutil -p dist/telegram-messages-sender.app/Contents/Info.plist
```

Measured on an Apple M5 Mac (`Mac17,2`, 16 GiB RAM) running macOS 26.5.1 (25F80). The release-style build used Go 1.26.3, Fyne 2.8.1, and Command Line Tools with the macOS 14.0 SDK. Both binaries reported `arm64` and Mach-O `minos 15.0`. The bundle declares `LSMinimumSystemVersion = 15.0`.

| Measurement | Development Mac result |
| --- | --- |
| Installed size (`du -sk`) | Bundle 62,336 KiB (60.9 MiB), CLI 5,560 KiB (5.4 MiB). |
| Idle CPU and memory | Median 0.9% CPU (0.7–1.0%) and 136,992 KiB RSS (133.8 MiB). Five `ps -o %cpu=,rss=` samples, two seconds apart, after a ten-second settle. One visible window, empty database, no Telegram authorization. |
| Process count | One desktop process and zero children at idle. |
| First launch | 1.28 s to a successful CLI status response and 1.72 s to detection of the visible window, measured from `open -n` with fresh state. |
| Relaunches | Two subsequent launches took 0.08–0.09 s to CLI status and 0.50–0.57 s to a visible window. |

The packaged app kept the original owner on a forced second launch. The CLI read status from that owner and returned its error for an unknown publication. Graceful shutdown removed the endpoint. After a forced exit the CLI rejected the stale endpoint, and a fresh launch replaced it. A separately signed test copy invoked the native AppKit last-window close action and verified that the CLI then reported the owner closed. The development shell lacked Accessibility permission for clicking the close button in the unmodified bundle.

An isolated, ad-hoc-signed test copy exercised the same Keychain adapter with disposable credentials and session data. Create, update, restart/reuse and access denial under a restricted ACL passed. The test entries were removed.

- **macOS 15 runtime** is pending because no macOS 15 Apple Silicon environment was available.
- **Live Telegram** login, session reuse, posting manual text and Saved Messages text, photo and video, observer verification of sender, forwarded attribution, captions and formatting, and update-history recovery remain pending.
