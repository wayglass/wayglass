# Run through scripts/demo/tapes.sh; setup is hidden, operations are real.
Output docs/tapes/out/propose.gif
Output docs/tapes/out/propose.mp4
Output docs/tapes/out/propose.txt

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
Type@1ms "source scripts/demo/tapes.sh; tape_setup propose >/dev/null; echo setup-''ready" Enter
Wait+Screen@300s /setup-ready/
Type@1ms "clear" Enter
Sleep 500ms
Show

Type "tape_propose docs; echo docs-''done" Enter
Wait+Screen /docs-done/
Sleep 3s
Type "clear" Enter
Type "tape_propose auth; echo auth-''done" Enter
Wait+Screen /auth-done/
Sleep 4s
Type "clear" Enter
Type "tape_propose pii; echo pii-''done" Enter
Wait+Screen /pii-done/
Sleep 4s
