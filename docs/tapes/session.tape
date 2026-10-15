# Run through scripts/demo/tapes.sh; setup is hidden, operations are real.
Output docs/tapes/out/session.gif
Output docs/tapes/out/session.mp4
Output docs/tapes/out/session.txt

Require bash
Require wayglass
Set Shell "bash"
Set FontSize 18
Set FontFamily "Menlo"
Set Width 1280
Set Height 720
Set Padding 16
Set Theme "Catppuccin Mocha"
Set TypingSpeed 35ms
Set WindowBar Colorful
Set Framerate 15
Set WaitTimeout 120s

Hide
Type@1ms "source scripts/demo/tapes.sh; tape_setup session >/dev/null; echo setup-''ready" Enter
Wait+Screen@300s /setup-ready/
Type@1ms "clear" Enter
Sleep 500ms
Show

Type "echo $SESSION_URL" Enter
Sleep 2s
Type "tape_fork; echo fork-''done" Enter
Wait+Screen /fork-done/
Sleep 4s
