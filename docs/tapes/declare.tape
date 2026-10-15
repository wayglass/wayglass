# Run through scripts/demo/tapes.sh; setup is hidden, operations are real.
Output docs/tapes/out/declare.gif
Output docs/tapes/out/declare.mp4
Output docs/tapes/out/declare.txt

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
Type@1ms "source scripts/demo/tapes.sh; tape_setup declare >/dev/null; echo setup-''ready" Enter
Wait+Screen@300s /setup-ready/
Type@1ms "clear" Enter
Sleep 500ms
Show

Type "tape_declare docs & p1=$!; tape_declare auth & p2=$!" Enter
Type "wait $p1; wait $p2; echo declarations-''done" Enter
Wait+Screen /declarations-done/
Sleep 4s
