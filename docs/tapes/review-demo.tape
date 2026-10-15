# Run through scripts/demo/tapes.sh; setup is hidden, operations are real.
Output docs/tapes/out/review-demo.gif
Output docs/tapes/out/review-demo.mp4
Output docs/tapes/out/review-demo.txt

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
Type@1ms "source scripts/demo/tapes.sh; tape_setup review-demo >/dev/null; echo setup-''ready" Enter
Wait+Screen@300s /setup-ready/
Type@1ms "clear" Enter
Sleep 500ms
Show

Type "wayglass review --demo-data i3f2a91c0d1" Enter
Wait+Screen /waiting for a person/
Sleep 3s
Type "i"
Wait+Screen /AI advice/
Sleep 4s
Type "a"
Wait+Screen /why\?/
Type "Sample review: checked the example login change and its declared paths."
Sleep 2s
Enter
Wait+Screen /approved i3f2a91/
Sleep 4s
Type "q"
Sleep 1s
