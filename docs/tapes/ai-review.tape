# Sample data: shows the AI surfaces without contacting a model or deciding live work.
Output docs/tapes/out/ai-review.mp4
Output docs/tapes/out/ai-review.webm
Output docs/tapes/out/ai-review.txt
Require bash
Require wayglass
Set Shell "bash"
Set FontSize 18
Set FontFamily "Menlo"
Set Width 1280
Set Height 720
Set Padding 16
Set Theme "Catppuccin Mocha"
Set TypingSpeed 30ms
Set WindowBar Colorful
Set Framerate 15
Set WaitTimeout 30s
Hide
Type@1ms "export PS1='$ '; clear" Enter
Sleep 500ms
Show
Type "wayglass review --demo-data i4c5d6e7f8a" Enter
Wait+Screen /AI escalated/
Sleep 4s
Screenshot docs/tapes/out/ai-review.png
Type "q"
Sleep 500ms
Type "wayglass review --demo-data i3f2a91c0d1" Enter
Wait+Screen /AI advice/
Sleep 4s
Type "J"
Sleep 3s
Type "q"
Sleep 500ms
Type "wayglass review --demo-data" Enter
Wait+Screen /waiting for a person/
Tab
Type "jjjj"
Sleep 4s
Type "q"
Sleep 1s
