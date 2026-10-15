# Run through scripts/demo/tapes.sh; setup is hidden, operations are real.
Output docs/tapes/out/watch-demo.gif
Output docs/tapes/out/watch-demo.mp4
Output docs/tapes/out/watch-demo.txt

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
Type@1ms "source scripts/demo/tapes.sh; tape_setup watch-demo >/dev/null; echo setup-''ready" Enter
Wait+Screen@300s /setup-ready/
Type@1ms "clear" Enter
Sleep 500ms
Show

Type "wayglass watch --demo-data --interval 500ms" Enter
Wait+Screen /wayglass watch/
Sleep 20s
Enter
Wait+Screen /declared against pushed/
Sleep 4s
Escape
Type "q"
Sleep 1s
