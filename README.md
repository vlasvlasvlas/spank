<p align="center">
  <img src="doc/logo.png" alt="spank logo" width="200">
</p>

# spank

**English** | [简体中文][readme-zh-link]

Slap your MacBook, it yells back.

> "this is the most amazing thing i've ever seen" — [@kenwheeler](https://x.com/kenwheeler)

> "I just ran sexy mode with my wife sitting next to me...We died laughing" — [@duncanthedev](https://x.com/duncanthedev)

> "peak engineering" — [@tylertaewook](https://x.com/tylertaewook)

Uses the Apple Silicon accelerometer (Bosch BMI286 IMU via IOKit HID) to detect physical hits on your laptop and plays audio responses. Single binary, no dependencies.

## Requirements

- macOS on Apple Silicon (any M-series chip M2 or greater, or the M1 Pro SKU specifically, no other M1/A-series chips!)
- `sudo` (for IOKit HID accelerometer access)
- Go 1.26+ (if building from source)

## Install

Download from the [latest release](https://github.com/taigrr/spank/releases/latest).

Or build from source:

```bash
go install github.com/taigrr/spank@latest
```

> **Note:** `go install` places the binary in `$GOBIN` (if set) or `$(go env GOPATH)/bin` (which defaults to `~/go/bin`). Copy it to a system path so `sudo spank` works. For example, with the default Go settings:
>
> ```bash
> sudo cp "$(go env GOPATH)/bin/spank" /usr/local/bin/spank
> ```

## Usage

spank now has two complementary modalities:

- **Classic**: the original CLI toy. Fast, minimal, no browser required.
- **SpankSynth**: a browser-based audiovisual layer on top of the same slap detector, with mixer controls, FX, synth, recordings, loops, and YAML presets.

```bash
# Classic mode — says "ow!" when slapped
sudo spank

# Sexy mode — escalating responses based on slap frequency
sudo spank --sexy

# Halo mode — plays Halo death sounds when slapped
sudo spank --halo

# Fast mode — faster polling and shorter cooldown
sudo spank --fast
sudo spank --sexy --fast

# Custom mode — plays your own MP3/WAV files from a directory
sudo spank --custom /path/to/mp3s

# SpankSynth mode — local browser UI with mixer, recordings, and YAML presets
sudo spank --instrument
sudo spank --instrument --config ./instrument.example.yaml
./spank_instrument.sh
./spank_instrument.sh ./instrument.example.yaml

# Adjust sensitivity with amplitude threshold (lower = more sensitive)
sudo spank --min-amplitude 0.1   # more sensitive
sudo spank --min-amplitude 0.25  # less sensitive
sudo spank --sexy --min-amplitude 0.2

# Set cooldown period in millisecond (default: 750)
sudo spank --cooldown 600

# Set playback speed multiplier (default: 1.0)
sudo spank --speed 0.7   # slower and deeper
sudo spank --speed 1.5   # faster
sudo spank --sexy --speed 0.6
```

### Modes

### Classic Mode

Classic mode is the original `spank` experience:

- accelerometer-driven slap detection in the terminal
- immediate audio playback from built-in packs or your own files
- no browser, no extra UI, no preset editing required

Run it with:

```bash
sudo spank
sudo spank --sexy
sudo spank --halo
sudo spank --lizard
sudo spank --custom /path/to/audio
```

**Pain mode** (default): Randomly plays from 10 pain/protest audio clips when a slap is detected.

**Sexy mode** (`--sexy`): Tracks slaps within a rolling 5-minute window. The more you slap, the more intense the audio response. 60 levels of escalation.

**Halo mode** (`--halo`): Randomly plays from death sound effects from the Halo video game series when a slap is detected.

**Custom mode** (`--custom`): Randomly plays MP3 or WAV files from a custom directory you specify.

**SpankSynth mode** (`--instrument`): Launches a local browser-based lab at `http://127.0.0.1:8765` with reactive visuals, live mixer controls, synth, FX, and managed recordings. The classic CLI detector remains unchanged; it is an additional surface layered on top.

### SpankSynth Mode

SpankSynth preserves the original accelerometer-driven behavior and adds:

- browser-based visual feedback with instant full-screen color impact
- separate master, WAV, and synth gain controls in the web UI
- editable hit threshold in the web UI and YAML
- global delay/reverb controls and a synth voice in the web UI
- loop-capable layers for persistent textures in instrument mode
- live microphone recording to managed `.wav` layers you can trim, preview, save, and delete
- YAML configuration for palettes, triggers, mixer, FX, and synth defaults
- support for both `.mp3` and `.wav` files in custom audio layers

Start it with:

```bash
sudo spank --instrument
sudo spank --instrument --config ./instrument.example.yaml
./spank_instrument.sh
./spank_instrument.sh ./instrument.example.yaml
```

Use [`instrument.example.yaml`](./instrument.example.yaml) as a starting point for residency or performance presets.
Set `loop: true` on any instrument layer in YAML to have that source latch as a texture until the combo state cools off.
The web UI can save the current instrument state back to YAML. If you launched without `--config`, it defaults to `./instrument.session.yaml`.

Current UI behavior:

- black fullscreen stage with reactive flash visuals
- minimal settings drawer behind the gear icon
- `master gain` at the top of audio controls
- `vols` accordion for WAV gain, synth gain, and hit threshold
- `wavs` accordion with mic recording, waveform view, trim start/end, preview, save, and delete
- flash editor with RGB color and fade-out time
- synth defaults to `sine`

Current flash defaults:

- white attack is fixed at `20ms`
- full color is reached at amplitude `0.05`
- only `fade out` is exposed in the UI

How it works:

1. `spank` still reads the accelerometer directly from Apple Silicon hardware.
2. In `instrument mode`, each detected slap is turned into an audiovisual event.
3. The Go backend serves a local web UI and streams impact events to it.
4. The UI reacts instantly with color and mixer feedback.
5. Audio layers, recordings, FX, synth settings, and loop behavior can be edited live and saved back to YAML.

### Detection tuning

Use `--fast` for a more responsive profile with faster polling (4ms vs 10ms), shorter cooldown (350ms vs 750ms), higher sensitivity (0.18 vs 0.05 threshold), and larger sample batch (320 vs 200).

You can still override individual values with `--min-amplitude` and `--cooldown` when needed.

### Sensitivity

Control detection sensitivity with `--min-amplitude` (default: `0.05`):

- Lower values (e.g., 0.05-0.10): Very sensitive, detects light taps
- Medium values (e.g., 0.15-0.30): Balanced sensitivity
- Higher values (e.g., 0.30-0.50): Only strong impacts trigger sounds

The value represents the minimum acceleration amplitude (in g-force) required to trigger a sound.

## Running as a Service

To have spank start automatically at boot, create a launchd plist. Pick your mode:

<details>
<summary>Pain mode (default)</summary>

```bash
sudo tee /Library/LaunchDaemons/com.taigrr.spank.plist > /dev/null << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.taigrr.spank</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/spank</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/spank.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/spank.err</string>
</dict>
</plist>
EOF
```

</details>

<details>
<summary>Sexy mode</summary>

```bash
sudo tee /Library/LaunchDaemons/com.taigrr.spank.plist > /dev/null << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.taigrr.spank</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/spank</string>
        <string>--sexy</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/spank.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/spank.err</string>
</dict>
</plist>
EOF
```

</details>

<details>
<summary>Halo mode</summary>

```bash
sudo tee /Library/LaunchDaemons/com.taigrr.spank.plist > /dev/null << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.taigrr.spank</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/spank</string>
        <string>--halo</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/spank.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/spank.err</string>
</dict>
</plist>
EOF
```

</details>

> **Note:** Update the path to `spank` if you installed it elsewhere (e.g. `~/go/bin/spank`).

Load and start the service:

```bash
sudo launchctl load /Library/LaunchDaemons/com.taigrr.spank.plist
```

Since the plist lives in `/Library/LaunchDaemons` and no `UserName` key is set, launchd runs it as root — no `sudo` needed.

To stop or unload:

```bash
sudo launchctl unload /Library/LaunchDaemons/com.taigrr.spank.plist
```

## How it works

1. Reads raw accelerometer data directly via IOKit HID (Apple SPU sensor)
2. Runs vibration detection (STA/LTA, CUSUM, kurtosis, peak/MAD)
3. When a significant impact is detected, plays an embedded MP3 response
4. **Optional volume scaling** (`--volume-scaling`) — light taps play quietly, hard slaps play at full volume
5. **Optional speed control** (`--speed`) — adjusts playback speed and pitch (0.5 = half speed, 2.0 = double speed)
6. 750ms cooldown between responses to prevent rapid-fire, adjustable with `--cooldown`

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=taigrr/spank&type=date&legend=top-left)](https://www.star-history.com/#taigrr/spank&type=date&legend=top-left)

## Credits

Sensor reading and vibration detection ported from [olvvier/apple-silicon-accelerometer](https://github.com/olvvier/apple-silicon-accelerometer).

## License

MIT

<!-- Links -->
[readme-zh-link]: ./README-zh.md
