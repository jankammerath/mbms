# Vintage QuickTime TV (vinqttv)

This ffmpeg command produces output that works with QuickTime Player 6 (1999).

```bash
ffmpeg -i input.mp4 -r 25 -vf scale=640:360 -b:v 320k -b:a 128k -ar 44100 -c:v svq1 -c:a aac -f mov output.mov
```