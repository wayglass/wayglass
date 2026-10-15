# Run through scripts/demo/tapes.sh; setup is hidden, operations are real.
Output docs/tapes/out/review.gif
Output docs/tapes/out/review.mp4
Output docs/tapes/out/review.txt

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
Type@1ms "source scripts/demo/tapes.sh; tape_setup review >/dev/null; echo setup-''ready" Enter
Wait+Screen@300s /setup-ready/
Type@1ms "clear" Enter
Sleep 500ms
Show

Type "wayglass review $AUTH_ID" Enter
Wait+Screen /waiting for a person/
Sleep 4s
Type "a"
Wait+Screen /why\?/
Type "Tested empty and whitespace usernames are rejected; alice is accepted."
Sleep 2s
Enter
Wait+Screen /approved/
Sleep 4s
Type "q"
Sleep 1s
